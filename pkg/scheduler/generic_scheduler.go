/*
Copyright 2014 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package scheduler

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/klog/v2"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/parallelize"
	"k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	internalcache "k8s.io/kubernetes/pkg/scheduler/internal/cache"
	"k8s.io/kubernetes/pkg/scheduler/metrics"
	utiltrace "k8s.io/utils/trace"
)

const (
	//minFeasibleNodesToFind是一个常量(100)，也就是说无论比例是多少，最少采样也要100个Node，除非Node总数就不足100个。
	// minFeasibleNodesToFind is the minimum number of nodes that would be scored
	// in each scheduling cycle. This is a semi-arbitrary value to ensure that a
	// certain minimum of nodes are checked for feasibility. This in turn helps
	// ensure a minimum level of spreading.
	minFeasibleNodesToFind = 100
	// minFeasibleNodesPercentageToFind is the minimum percentage of nodes that
	// would be scored in each scheduling cycle. This is a semi-arbitrary value
	// to ensure that a certain minimum of nodes are checked for feasibility.
	// This in turn helps ensure a minimum level of spreading.
	minFeasibleNodesPercentageToFind = 5
)

// ErrNoNodesAvailable is used to describe the error that no nodes available to schedule pods.
var ErrNoNodesAvailable = fmt.Errorf("no nodes available to schedule pods")

// ScheduleAlgorithm is an interface implemented by things that know how to schedule pods
// onto machines.
// TODO: Rename this type.
type ScheduleAlgorithm interface {
	Schedule(context.Context, []framework.Extender, framework.Framework, *framework.CycleState, *v1.Pod) (scheduleResult ScheduleResult, err error)
}

// ScheduleResult represents the result of one pod scheduled. It will contain
// the final selected Node, along with the selected intermediate information.
type ScheduleResult struct {
	// Name of the scheduler suggest host
	SuggestedHost string
	// Number of nodes scheduler evaluated on one pod scheduled
	EvaluatedNodes int
	// Number of feasible nodes on one pod scheduled
	FeasibleNodes int
}

type genericScheduler struct {
	// 缓存
	cache                    internalcache.Cache
        // 缓存快照
	nodeInfoSnapshot         *internalcache.Snapshot
	// 比如一共有1000个Node，如果1000个都是可行Node，那么就需要计算1000个Node的分数，这可能是一个比较大的计算量。
        // 真的需要1000个Node都参与评分么？不一定，可能100个就够了，有的时候不需要找到全局的最优解，局部最优也是不错的，只要这个局部数量别太少。
	percentageOfNodesToScore int32
	// 就是因为percentageOfNodesToScore的存在，可能只要一部分Node参与调度。
        // 那么下次调度的时候希望不要一直在同一批Node上调度，直到有Node过滤失败再有新的Node加入进来。
        // nextStartNodeIndex就是用来记录下一次调度的时候从哪个Node开始遍历，每次调度完成后nextStartNodeIndex需要加上此次调度遍历Node的数量。
        // 这样调度下一个Pod的时候就从下一批Node开始，当然每次更新nextStartNodeIndex的时候需要考虑超出范围归零的问题。
	nextStartNodeIndex       int
}

// snapshot snapshots scheduler cache and node infos for all fit and priority
// functions.
func (g *genericScheduler) snapshot() error {
	// Used for all fit and priority funcs.
	return g.cache.UpdateSnapshot(g.nodeInfoSnapshot)
}

// Schedule tries to schedule the given pod to one of the nodes in the node list.
// If it succeeds, it will return the name of the node.
// If it fails, it will return a FitError error with reasons.
func (g *genericScheduler) Schedule(ctx context.Context, extenders []framework.Extender, fwk framework.Framework, state *framework.CycleState, pod *v1.Pod) (result ScheduleResult, err error) {
	trace := utiltrace.New("Scheduling", utiltrace.Field{Key: "namespace", Value: pod.Namespace}, utiltrace.Field{Key: "name", Value: pod.Name})
	defer trace.LogIfLong(100 * time.Millisecond)
        // 对cache中的nodes进行打快照，避免在调度过程中cache的变化，引发调度的不一致
	if err := g.snapshot(); err != nil {
		return result, err
	}
	trace.Step("Snapshotting scheduler cache and node infos done")
        // 如果没有节点信息直接返回
	if g.nodeInfoSnapshot.NumNodes() == 0 {
		return result, ErrNoNodesAvailable
	}
        // 为pod 找出适合的节点，diagnosis 表示调度结论，多少节点参与调度，以及在哪个插件调度失败等信息
	feasibleNodes, diagnosis, err := g.findNodesThatFitPod(ctx, extenders, fwk, state, pod)
	if err != nil {
		return result, err
	}
	trace.Step("Computing predicates done")
        
	// 没有合适的节点
	if len(feasibleNodes) == 0 {
		return result, &framework.FitError{
			Pod:         pod,
			NumAllNodes: g.nodeInfoSnapshot.NumNodes(),
			Diagnosis:   diagnosis,
		}
	}
        
	// 只选出一个合适的节点，就没有必要进行打分了，直接返回
	// When only one node after predicate, just use it.
	if len(feasibleNodes) == 1 {
		return ScheduleResult{
			SuggestedHost:  feasibleNodes[0].Name,
			EvaluatedNodes: 1 + len(diagnosis.NodeToStatusMap),
			FeasibleNodes:  1,
		}, nil
	}
        
	// 对适合节点，进行评分，同时返回NodeScores
	priorityList, err := prioritizeNodes(ctx, extenders, fwk, state, pod, feasibleNodes)
	if err != nil {
		return result, err
	}
        // 选出评分最高的节点
	host, err := g.selectHost(priorityList)
	trace.Step("Prioritizing done")

	return ScheduleResult{
		SuggestedHost:  host,
		EvaluatedNodes: len(feasibleNodes) + len(diagnosis.NodeToStatusMap),
		FeasibleNodes:  len(feasibleNodes),
	}, err
}

// selectHost()根据所有可行Node的评分找到最优的Node
// selectHost takes a prioritized list of nodes and then picks one
// in a reservoir sampling manner from the nodes that had the highest score.
func (g *genericScheduler) selectHost(nodeScoreList framework.NodeScoreList) (string, error) {
	// 没有可行Node的评分，返回错误
	if len(nodeScoreList) == 0 {
		return "", fmt.Errorf("empty priorityList")
	}
	// 在nodeScoreList中找到分数最高的Node，初始化第0个Node分数最高
	maxScore := nodeScoreList[0].Score
	selected := nodeScoreList[0].Name
	cntOfMaxScore := 1
	for _, ns := range nodeScoreList[1:] {
		if ns.Score > maxScore {
			maxScore = ns.Score
			selected = ns.Name
			cntOfMaxScore = 1
		} else if ns.Score == maxScore {
			// 当节点的分数相同时，用[0,cntOfMaxScore)的随机数表示节点有1/cntOfMaxScore概率成为最优节点
			cntOfMaxScore++
			if rand.Intn(cntOfMaxScore) == 0 {
				// Replace the candidate with probability of 1/cntOfMaxScore
				selected = ns.Name
			}
		}
	}
	return selected, nil
}

// 在节点规模比较大的时候，大于1000时，可以根据percentageOfNodesToScore进行采样，MAX（numAllNodes * adaptivePercentage / 100，minFeasibleNodesToFind=100）
// numFeasibleNodesToFind returns the number of feasible nodes that once found, the scheduler stops
// its search for more feasible nodes.
func (g *genericScheduler) numFeasibleNodesToFind(numAllNodes int32) (numNodes int32) {
	// minFeasibleNodesToFind是一个常量(100)，也就是说无论比例是多少，最少也要100个Node，除非Node总数就不足100个。
        // g.percentageOfNodesToScore >= 100等于比例无效，无论多少Node都可以
	if numAllNodes < minFeasibleNodesToFind || g.percentageOfNodesToScore >= 100 {
		return numAllNodes
	}
	
	adaptivePercentage := g.percentageOfNodesToScore
	if adaptivePercentage <= 0 {
		// 基础比例是50%
		basePercentageOfNodesToScore := int32(50)
               // Node总数满125个比例减一，可以持续累加，这个跟双十一满减一个原理，至于为什么是125笔者还没弄清楚。
               // 这可以理解，单纯的比例并不合理，尤其是Node总量非常大的情况下，比例应该适当调低。
               // 所以可以用函数y=(50x-0.008x^2)/100表示最终Node数量。初中数据告诉我们这是一个开口朝下的抛物线。
               // 这个函数有一个最大值，是当x=-b/2a，即Node总量为3125个时，应该选择782个Node。
              // 也就是说，当Node总量在[100(前面过滤了小于100的情况), 3125]时，选择Node的数量是在[49，782]范围单调递增的；
              // 当Node数量[3125, 7250]时，选择Node的数量是在[782, 0]单点递减的，超过7250比例就会变为负数。
		adaptivePercentage = basePercentageOfNodesToScore - numAllNodes/125
		// 比例总不能<=0吧？所以就有了minFeasibleNodesPercentageToFind(5)，即最小比例也应该是5%
		if adaptivePercentage < minFeasibleNodesPercentageToFind {
			adaptivePercentage = minFeasibleNodesPercentageToFind
		}
	}
        // 按照比例计算Node数量，adaptivePercentage = 50 - 0.008*numAllNodes
	// 所以numNodes = (50*numAllNodes - 0.008*numAllNodes^2) / 100，就是上面y=(50x-0.008x^2)/100的公式
	numNodes = numAllNodes * adaptivePercentage / 100
	if numNodes < minFeasibleNodesToFind {
		// 最终还要限制在>=100范围，因为太少会影响调度效果
		return minFeasibleNodesToFind
	}

	return numNodes
}

func (g *genericScheduler) evaluateNominatedNode(ctx context.Context, extenders []framework.Extender, pod *v1.Pod, fwk framework.Framework, state *framework.CycleState, diagnosis framework.Diagnosis) ([]*v1.Node, error) {
	nnn := pod.Status.NominatedNodeName
	nodeInfo, err := g.nodeInfoSnapshot.Get(nnn)
	if err != nil {
		return nil, err
	}
	node := []*framework.NodeInfo{nodeInfo}
	feasibleNodes, err := g.findNodesThatPassFilters(ctx, fwk, state, pod, diagnosis, node)
	if err != nil {
		return nil, err
	}

	feasibleNodes, err = findNodesThatPassExtenders(extenders, pod, feasibleNodes, diagnosis.NodeToStatusMap)
	if err != nil {
		return nil, err
	}

	return feasibleNodes, nil
}

// Filters the nodes to find the ones that fit the pod based on the framework
// filter plugins and filter extenders.
func (g *genericScheduler) findNodesThatFitPod(ctx context.Context, extenders []framework.Extender, fwk framework.Framework, state *framework.CycleState, pod *v1.Pod) ([]*v1.Node, framework.Diagnosis, error) {
	// 初始化调度结果的对象
	diagnosis := framework.Diagnosis{
		NodeToStatusMap:      make(framework.NodeToStatusMap),
		UnschedulablePlugins: sets.NewString(),
	}
        
        // 注意 前置过滤是不会传入节点的
	// Run "prefilter" plugins.
	s := fwk.RunPreFilterPlugins(ctx, state, pod)
	allNodes, err := g.nodeInfoSnapshot.NodeInfos().List()
	if err != nil {
		return nil, diagnosis, err
	}
	if !s.IsSuccess() {
		if !s.IsUnschedulable() {
			return nil, diagnosis, s.AsError()
		}
		// All nodes will have the same status. Some non trivial refactoring is
		// needed to avoid this copy.
		for _, n := range allNodes {
			diagnosis.NodeToStatusMap[n.Node().Name] = s
		}
		// Status satisfying IsUnschedulable() gets injected into diagnosis.UnschedulablePlugins.
		diagnosis.UnschedulablePlugins.Insert(s.FailedPlugin())
		return nil, diagnosis, nil
	}
         
	// 由于抢占调度有可能在先前的调度周期中设置Pod.Status.NominatedNodeName。
	// 该Node可能是唯一适合该Pod的候选Node，因此，在遍历所有Node之前先尝试提名的Node
	// "NominatedNodeName" can potentially be set in a previous scheduling cycle as a result of preemption.
	// This node is likely the only candidate that will fit the pod, and hence we try it first before iterating over all nodes.
	if len(pod.Status.NominatedNodeName) > 0 {
		feasibleNodes, err := g.evaluateNominatedNode(ctx, extenders, pod, fwk, state, diagnosis)
		if err != nil {
			klog.ErrorS(err, "Evaluation failed on nominated node", "pod", klog.KObj(pod), "node", pod.Status.NominatedNodeName)
		}
		// Nominated node passes all the filters, scheduler is good to assign this node to the pod.
		if len(feasibleNodes) != 0 {
			return feasibleNodes, diagnosis, nil
		}
	}
	feasibleNodes, err := g.findNodesThatPassFilters(ctx, fwk, state, pod, diagnosis, allNodes)
	if err != nil {
		return nil, diagnosis, err
	}

	feasibleNodes, err = findNodesThatPassExtenders(extenders, pod, feasibleNodes, diagnosis.NodeToStatusMap)
	if err != nil {
		return nil, diagnosis, err
	}
	return feasibleNodes, diagnosis, nil
}

// findNodesThatPassFilters finds the nodes that fit the filter plugins.
func (g *genericScheduler) findNodesThatPassFilters(
	ctx context.Context,
	fwk framework.Framework,
	state *framework.CycleState,
	pod *v1.Pod,
	diagnosis framework.Diagnosis,
	nodes []*framework.NodeInfo) ([]*v1.Node, error) {
        // 取样出需要过滤的节点数量
	numNodesToFind := g.numFeasibleNodesToFind(int32(len(nodes)))

	// Create feasible list with enough space to avoid growing it
	// and allow assigning.
	feasibleNodes := make([]*v1.Node, numNodesToFind)
	// 如果没有过滤插件直接根据nextStartNodeIndex 从所有的node中取出numNodesToFind个节点
	if !fwk.HasFilterPlugins() {
		length := len(nodes)
		for i := range feasibleNodes {
			feasibleNodes[i] = nodes[(g.nextStartNodeIndex+i)%length].Node()
		}
		g.nextStartNodeIndex = (g.nextStartNodeIndex + len(feasibleNodes)) % length
		return feasibleNodes, nil
	}

	errCh := parallelize.NewErrorChannel()
	var statusesLock sync.Mutex
	var feasibleNodesLen int32
	ctx, cancel := context.WithCancel(ctx)
	// 从取样的节点集合中取出节点，同时跑过滤插件
	checkNode := func(i int) {
		// We check the nodes starting from where we left off in the previous scheduling cycle,
		// this is to make sure all nodes have the same chance of being examined across pods.
		nodeInfo := nodes[(g.nextStartNodeIndex+i)%len(nodes)]
		status := fwk.RunFilterPluginsWithNominatedPods(ctx, state, pod, nodeInfo)
		if status.Code() == framework.Error {
			errCh.SendErrorWithCancel(status.AsError(), cancel)
			return
		}
		if status.IsSuccess() {
			length := atomic.AddInt32(&feasibleNodesLen, 1)
			if length > numNodesToFind {
				cancel()
				atomic.AddInt32(&feasibleNodesLen, -1)
			} else {
				feasibleNodes[length-1] = nodeInfo.Node()
			}
		} else {
			statusesLock.Lock()
			diagnosis.NodeToStatusMap[nodeInfo.Node().Name] = status
			diagnosis.UnschedulablePlugins.Insert(status.FailedPlugin())
			statusesLock.Unlock()
		}
	}

	beginCheckNode := time.Now()
	statusCode := framework.Success
	defer func() {
		// We record Filter extension point latency here instead of in framework.go because framework.RunFilterPlugins
		// function is called for each node, whereas we want to have an overall latency for all nodes per scheduling cycle.
		// Note that this latency also includes latency for `addNominatedPods`, which calls framework.RunPreFilterAddPod.
		metrics.FrameworkExtensionPointDuration.WithLabelValues(runtime.Filter, statusCode.String(), fwk.ProfileName()).Observe(metrics.SinceInSeconds(beginCheckNode))
	}()
        
	// 启动多个协程并行过滤Node，此处需要注意的是并不是启动了len(allNodes)协程，parallelize包有一个并行度，默认值是16.
        // 如果并行度是16，那么最多启动16个协程，每个协程负责过滤一部分Node，就是典型的MapReduce模型。
        // 举个例子，如果总共有64个Node，那么每个协程负责过滤4个连续的Node。
	// Stops searching for more nodes once the configured number of feasible nodes
	// are found.
	fwk.Parallelizer().Until(ctx, len(nodes), checkNode)
	processedNodes := int(feasibleNodesLen) + len(diagnosis.NodeToStatusMap)
	g.nextStartNodeIndex = (g.nextStartNodeIndex + processedNodes) % len(nodes)
        
	feasibleNodes = feasibleNodes[:feasibleNodesLen]
	if err := errCh.ReceiveError(); err != nil {
		statusCode = framework.Error
		return nil, err
	}
	return feasibleNodes, nil
}

// findNodesThatPassExtenders()利用Extender过滤Node
func findNodesThatPassExtenders(extenders []framework.Extender, pod *v1.Pod, feasibleNodes []*v1.Node, statuses framework.NodeToStatusMap) ([]*v1.Node, error) {
	
	// 遍历Extender, Extender是顺序调用的，而不是并发调用的。在一个Extender中排除一些可行Node，然后以递减的方式传递给下一个Extender。
	// 这种设计虽然可能降低Extender的传递Node的数据量，但是串行执行远程调用效率确实不可观。
	// Extenders are called sequentially.
	// Nodes in original feasibleNodes can be excluded in one extender, and pass on to the next
	// extender in a decreasing manner.
	for _, extender := range extenders {
		// 可行的Node数量为0就没必要过滤了
		if len(feasibleNodes) == 0 {
			break
		}
		// Pod申请的资源不是Extender管理的也就不需要用它来过滤，这也是一种有效的提升效率的方法，尽量避免无效的远程调用。比如：Extender扩展了GPU，但是这个pod申请资源并没有
		if !extender.IsInterested(pod) {
			continue
		}
              
		// Status of failed nodes in failedAndUnresolvableMap will be added or overwritten in <statuses>,
		// so that the scheduler framework can respect the UnschedulableAndUnresolvable status for
		// particular nodes, and this may eventually improve preemption efficiency.
		// Note: users are recommended to configure the extenders that may return UnschedulableAndUnresolvable
		// status ahead of others.
		feasibleList, failedMap, failedAndUnresolvableMap, err := extender.Filter(pod, feasibleNodes)
		if err != nil {
			// 如果Extender是可以忽略的，就会忽略Extender报错，也就是说调度Pod有没有该Extender参与都行，有自然更好。
			if extender.IsIgnorable() {
				klog.InfoS("Skipping extender as it returned error and has ignorable flag set", "extender", extender, "err", err)
				continue
			}
			return nil, err
		}
		
                // failAndUnresolvableMap中失败节点的状态会添加或覆盖到statuses。
		// 以便调度框架可以知悉特定节点的UnschedulableAndUnresolvable状态，这最终可以提高抢占效率。
		for failedNodeName, failedMsg := range failedAndUnresolvableMap {
			var aggregatedReasons []string
			if _, found := statuses[failedNodeName]; found {
				aggregatedReasons = statuses[failedNodeName].Reasons()
			}
			aggregatedReasons = append(aggregatedReasons, failedMsg)
			statuses[failedNodeName] = framework.NewStatus(framework.UnschedulableAndUnresolvable, aggregatedReasons...)
		}
		// 追加Extender过滤失败的原因
		for failedNodeName, failedMsg := range failedMap {
			if _, found := failedAndUnresolvableMap[failedNodeName]; found {
				// failedAndUnresolvableMap优先于failedMap，请注意，仅当Extender在两个map中都返回Node时，才会发生这种情况
				// failedAndUnresolvableMap takes precedence over failedMap
				// note that this only happens if the extender returns the node in both maps
				continue
			}
			if _, found := statuses[failedNodeName]; !found {
				statuses[failedNodeName] = framework.NewStatus(framework.Unschedulable, failedMsg)
			} else {
				statuses[failedNodeName].AppendReason(failedMsg)
			}
		}
                // Extender过滤通过的Node作为下一个Extender过滤的候选         
		feasibleNodes = feasibleList
	}
	return feasibleNodes, nil
}

// prioritizeNodes prioritizes the nodes by running the score plugins,
// which return a score for each node from the call to RunScorePlugins().
// The scores from each plugin are added together to make the score for that node, then
// any extenders are run as well.
// All scores are finally combined (added) to get the total weighted scores of all nodes
func prioritizeNodes(
	ctx context.Context,
	extenders []framework.Extender,
	fwk framework.Framework,
	state *framework.CycleState,
	pod *v1.Pod,
	nodes []*v1.Node,
) (framework.NodeScoreList, error) {
	// If no priority configs are provided, then all nodes will have a score of one.
	// This is required to generate the priority list in the required format
	if len(extenders) == 0 && !fwk.HasScorePlugins() {
		result := make(framework.NodeScoreList, 0, len(nodes))
		for i := range nodes {
			result = append(result, framework.NodeScore{
				Name:  nodes[i].Name,
				Score: 1,
			})
		}
		return result, nil
	}
        // 运行所有PreScorePlugin准备评分数据
	// Run PreScore plugins.
	preScoreStatus := fwk.RunPreScorePlugins(ctx, state, pod, nodes)
	if !preScoreStatus.IsSuccess() {
		return nil, preScoreStatus.AsError()
	}
        
	// 运行所有ScorePlugin对Node进行评分，此处需要知道的是scoresMap的类型是map[string][]NodeScore。
        // scoresMap的key是插件名字，value是该插件对所有Node的评分
	// Run the Score plugins.
	scoresMap, scoreStatus := fwk.RunScorePlugins(ctx, state, pod, nodes)
	if !scoreStatus.IsSuccess() {
		return nil, scoreStatus.AsError()
	}
        
	// Additional details logged at level 10 if enabled.
	klogV := klog.V(10)
	if klogV.Enabled() {
		for plugin, nodeScoreList := range scoresMap {
			for _, nodeScore := range nodeScoreList {
				klogV.InfoS("Plugin scored node for pod", "pod", klog.KObj(pod), "plugin", plugin, "node", nodeScore.Name, "score", nodeScore.Score)
			}
		}
	}

	// Summarize all scores.
	result := make(framework.NodeScoreList, 0, len(nodes))
        // 计算每个插件对每个节点跑分的结果
	for i := range nodes {
		result = append(result, framework.NodeScore{Name: nodes[i].Name, Score: 0})
		for j := range scoresMap {
			result[i].Score += scoresMap[j][i].Score
		}
	}
         

	if len(extenders) != 0 && nodes != nil {
		var mu sync.Mutex
		var wg sync.WaitGroup
		combinedScores := make(map[string]int64, len(nodes))
		for i := range extenders {
			if !extenders[i].IsInterested(pod) {
				continue
			}
			wg.Add(1)
			go func(extIndex int) {
				metrics.SchedulerGoroutines.WithLabelValues(metrics.PrioritizingExtender).Inc()
				defer func() {
					metrics.SchedulerGoroutines.WithLabelValues(metrics.PrioritizingExtender).Dec()
					wg.Done()
				}()
				prioritizedList, weight, err := extenders[extIndex].Prioritize(pod, nodes)
				if err != nil {
					// Prioritization errors from extender can be ignored, let k8s/other extenders determine the priorities
					klog.V(5).InfoS("Failed to run extender's priority function. No score given by this extender.", "error", err, "pod", klog.KObj(pod), "extender", extenders[extIndex].Name())
					return
				}
				mu.Lock()
				for i := range *prioritizedList {
					host, score := (*prioritizedList)[i].Host, (*prioritizedList)[i].Score
					if klogV.Enabled() {
						klogV.InfoS("Extender scored node for pod", "pod", klog.KObj(pod), "extender", extenders[extIndex].Name(), "node", host, "score", score)
					}
					combinedScores[host] += score * weight
				}
				mu.Unlock()
			}(i)
		}
		// wait for all go routines to finish
		wg.Wait()
		for i := range result {
			// MaxExtenderPriority may diverge from the max priority used in the scheduler and defined by MaxNodeScore,
			// therefore we need to scale the score returned by extenders to the score range used by the scheduler.
			result[i].Score += combinedScores[result[i].Name] * (framework.MaxNodeScore / extenderv1.MaxExtenderPriority)
		}
	}

	if klogV.Enabled() {
		for i := range result {
			klogV.InfoS("Calculated node's final score for pod", "pod", klog.KObj(pod), "node", result[i].Name, "score", result[i].Score)
		}
	}
	return result, nil
}

// NewGenericScheduler creates a genericScheduler object.
func NewGenericScheduler(
	cache internalcache.Cache,
	nodeInfoSnapshot *internalcache.Snapshot,
	percentageOfNodesToScore int32) ScheduleAlgorithm {
	return &genericScheduler{
		cache:                    cache,
		nodeInfoSnapshot:         nodeInfoSnapshot,
		percentageOfNodesToScore: percentageOfNodesToScore,
	}
}
