/*
Copyright 2015 The Kubernetes Authors.

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

package cache

import (
	"fmt"
	"os"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/metrics"
)

/*** Cache是缓存已经被调度和假定被调度的pod信息的****/

var (
	cleanAssumedPeriod = 1 * time.Second
)

// New returns a Cache implementation.
// It automatically starts a go routine that manages expiration of assumed pods.
// "ttl" is how long the assumed pod will get expired.
// "stop" is the channel that would close the background goroutine.
func New(ttl time.Duration, stop <-chan struct{}) Cache {
	cache := newSchedulerCache(ttl, cleanAssumedPeriod, stop)
	cache.run()
	return cache
}

// nodeInfoListItem holds a NodeInfo pointer and acts as an item in a doubly
// linked list. When a NodeInfo is updated, it goes to the head of the list.
// The items closer to the head are the most recently updated items.
type nodeInfoListItem struct {
	info *framework.NodeInfo
	next *nodeInfoListItem
	prev *nodeInfoListItem
}

type schedulerCache struct {
	stop   <-chan struct{}
	// assume的Pod一旦完成绑定，就要在指定的时间内确认，否则就会超时，ttl就是指定的过期时间，默认30秒
	ttl    time.Duration
	// 定时清理“假定过期”的Pod，period就是定时周期，默认是1秒钟
        // 前面提到了schedulerCache有自己的协程，就是定时清理超时的假定Pod.
	period time.Duration

	// This mutex guards all fields within this cache struct.
	mu sync.RWMutex
	// 假定Pod集合，map的key与podStates相同，都是Pod的NS+NAME，值为true就是假定Pod
	// a set of assumed pod keys.
	// The key could further be used to get an entry in podStates.
	assumedPods sets.String
	// podState继承了Pod的API定义，增加了Cache需要的属性
        //在原生的pod根据缓存的特性进行封装
	// a map from pod key to podState.
	podStates map[string]*podState
	// 所有的Node，键是Node.Name，值是nodeInfoList链表的item
	nodes     map[string]*nodeInfoListItem
	// 所有的Node再通过双向链表连接起来
	// headNode points to the most recently updated NodeInfo in "nodes". It is the
	// head of the linked list.
	headNode *nodeInfoListItem
	// 节点按照zone组织成树状，前面提到用nodeTree中Node的名字再到nodes中就可以查找到NodeInfo.
	nodeTree *nodeTree
	// 镜像状态，统计镜像的信息
	// A map from image name to its imageState.
	imageStates map[string]*imageState
}

// podState与继承了Pod的API类型定义，同时扩展了schedulerCache需要的属性.
type podState struct {
	pod *v1.Pod
	// 假定Pod的超时截止时间，用于判断假定Pod是否过期
	// Used by assumedPod to determinate expiration.
	deadline *time.Time
	// bindingFinished就是用于标记已经Bind完成的Pod，然后开始计时，计时的方法就是设置deadline
	// Used to block cache from expiring assumedPod if binding still runs
	bindingFinished bool
}

// 镜像的状态
type imageState struct {
	// Size of the image
	size int64
	// A set of node names for nodes having this image present
	nodes sets.String
}

// 节点上镜像信息和大小的总和
// createImageStateSummary returns a summarizing snapshot of the given image's state.
func (cache *schedulerCache) createImageStateSummary(state *imageState) *framework.ImageStateSummary {
	return &framework.ImageStateSummary{
		Size:     state.size,
		NumNodes: len(state.nodes),
	}
}

func newSchedulerCache(ttl, period time.Duration, stop <-chan struct{}) *schedulerCache {
	return &schedulerCache{
		ttl:    ttl,
		period: period,
		stop:   stop,

		nodes:       make(map[string]*nodeInfoListItem),
		nodeTree:    newNodeTree(nil),
		assumedPods: make(sets.String),
		podStates:   make(map[string]*podState),
		imageStates: make(map[string]*imageState),
	}
}

// newNodeInfoListItem initializes a new nodeInfoListItem.
func newNodeInfoListItem(ni *framework.NodeInfo) *nodeInfoListItem {
	return &nodeInfoListItem{
		info: ni,
	}
}

// 节点的信息每变化一次，将nodeinfo 放到双向链表的头部，会根据版本的数值，进行增量的更新快照
// moveNodeInfoToHead moves a NodeInfo to the head of "cache.nodes" doubly
// linked list. The head is the most recently updated NodeInfo.
// We assume cache lock is already acquired.
func (cache *schedulerCache) moveNodeInfoToHead(name string) {
	ni, ok := cache.nodes[name]
	if !ok {
		klog.ErrorS(nil, "No node info with given name found in the cache", "node", klog.KRef("", name))
		return
	}
	// if the node info list item is already at the head, we are done.
	if ni == cache.headNode {
		return
	}
        // 将变更的nodeInfo 的item 进行链表的移除
	if ni.prev != nil {
		ni.prev.next = ni.next
	}
	if ni.next != nil {
		ni.next.prev = ni.prev
	}
	if cache.headNode != nil {
		cache.headNode.prev = ni
	}
	// 插入表头
	ni.next = cache.headNode
	ni.prev = nil
	cache.headNode = ni
}

// 节点被删除，从双向链表中移除
// removeNodeInfoFromList removes a NodeInfo from the "cache.nodes" doubly
// linked list.
// We assume cache lock is already acquired.
func (cache *schedulerCache) removeNodeInfoFromList(name string) {
	ni, ok := cache.nodes[name]
	if !ok {
		klog.ErrorS(nil, "No node info with given name found in the cache", "node", klog.KRef("", name))
		return
	}

	if ni.prev != nil {
		ni.prev.next = ni.next
	}
	if ni.next != nil {
		ni.next.prev = ni.prev
	}
	// if the removed item was at the head, we must update the head.
	if ni == cache.headNode {
		cache.headNode = ni.next
	}
	delete(cache.nodes, name)
}

// Dump produces a dump of the current scheduler cache. This is used for
// debugging purposes only and shouldn't be confused with UpdateSnapshot
// function.
// This method is expensive, and should be only used in non-critical path.
func (cache *schedulerCache) Dump() *Dump {
	cache.mu.RLock()
	defer cache.mu.RUnlock()

	nodes := make(map[string]*framework.NodeInfo, len(cache.nodes))
	for k, v := range cache.nodes {
		nodes[k] = v.info.Clone()
	}

	return &Dump{
		Nodes:       nodes,
		AssumedPods: cache.assumedPods.Union(nil),
	}
}

// 更新的是参数nodeSnapshot，不是更新Cache.
// 也就是Cache需要找到当前与nodeSnapshot的差异，然后更新它，这样nodeSnapshot就与Cache状态一致了
// UpdateSnapshot takes a snapshot of cached NodeInfo map. This is called at
// beginning of every scheduling cycle.
// The snapshot only includes Nodes that are not deleted at the time this function is called.
// nodeinfo.Node() is guaranteed to be not nil for all the nodes in the snapshot.
// This function tracks generation number of NodeInfo and updates only the
// entries of an existing snapshot that have changed after the snapshot was taken.
func (cache *schedulerCache) UpdateSnapshot(nodeSnapshot *Snapshot) error {
	cache.mu.Lock()
	defer cache.mu.Unlock()

        // 此处需要多说一点：kube-scheudler为Node定义了全局的generation变量，每个Node状态变化都会造成generation+=1然后赋值给该Node
        // nodeSnapshot.generation就是最新NodeInfo.Generation，就是表头的那个NodeInfo
	// Get the last generation of the snapshot.
	snapshotGeneration := nodeSnapshot.generation
	// Snapshot中有三个列表，分别是全量、亲和性和反亲和性列表
        // 全量列表在没有Node添加或者删除的时候，是不需要更新的
	// NodeInfoList and HavePodsWithAffinityNodeInfoList must be re-created if a node was added
	// or removed from the cache.
	updateAllLists := false
	// 当有Node的亲和性状态发生了变化(以前没有任何Pod有亲和性声明现在有了，抑或反过来)，
        // 则需要更新快照中的亲和性列表
	// HavePodsWithAffinityNodeInfoList must be re-created if a node changed its
	// status from having pods with affinity to NOT having pods with affinity or the other
	// way around.
	updateNodesHavePodsWithAffinity := false
	// 同上
	// HavePodsWithRequiredAntiAffinityNodeInfoList must be re-created if a node changed its
	// status from having pods with required anti-affinity to NOT having pods with required
	// anti-affinity or the other way around.
	updateNodesHavePodsWithRequiredAntiAffinity := false
	
	// 双向链的增量遍历，同时更新
	// 遍历Node列表，为什么不遍历Node的map？因为Node列表是按照Generation排序的
       // 只要找到大于nodeSnapshot.generation的所有Node然后把他们更新到nodeSnapshot中就可以了
	// Start from the head of the NodeInfo doubly linked list and update snapshot
	// of NodeInfos updated after the last snapshot.
	for node := cache.headNode; node != nil; node = node.next {
		if node.info.Generation <= snapshotGeneration {
			// all the nodes are updated before the existing snapshot. We are done.
			break
		}
		// node.info.Node()获取*v1.Node，前文说了，如果Node被删除，那么该值就是为nil
               // 所以只有未被删除的Node才会被更新到nodeSnapshot，因为快照中的全量Node列表是按照nodeTree排序的
               // 而nodeTree都是真实的node
		if np := node.info.Node(); np != nil {
			// 如果nodeSnapshot中没有该Node，则在nodeSnapshot中创建Node，并标记更新全量列表，因为创建了新的Node
			existing, ok := nodeSnapshot.nodeInfoMap[np.Name]
			if !ok {
				updateAllLists = true
				existing = &framework.NodeInfo{}
				nodeSnapshot.nodeInfoMap[np.Name] = existing
			}
			// 克隆NodeInfo，这个比较好理解，肯定不能简单的把指针设置过去，这样会造成多协程读写同一个对象
                        // 因为克隆操作比较重，所以能少做就少做，这也是利用Generation实现增量更新的原因
			clone := node.info.Clone()
			// We track nodes that have pods with affinity, here we check if this node changed its
			// status from having pods with affinity to NOT having pods with affinity or the other
			// way around.
			// 如果Pod以前或者现在有任何亲和性声明，则需要更新nodeSnapshot中的亲和性列表
			if (len(existing.PodsWithAffinity) > 0) != (len(clone.PodsWithAffinity) > 0) {
				updateNodesHavePodsWithAffinity = true
			}
			// 同上，需要更新非亲和性列表
			if (len(existing.PodsWithRequiredAntiAffinity) > 0) != (len(clone.PodsWithRequiredAntiAffinity) > 0) {
				updateNodesHavePodsWithRequiredAntiAffinity = true
			}
			 // 将NodeInfo的拷贝更新到nodeSnapshot中
			// We need to preserve the original pointer of the NodeInfo struct since it
			// is used in the NodeInfoList, which we may not update.
			*existing = *clone
		}
	}
	// Cache的表头Node的版本是最新的，所以也就代表了此时更新镜像后镜像的版本了
	// Update the snapshot generation with the latest NodeInfo generation.
	if cache.headNode != nil {
		nodeSnapshot.generation = cache.headNode.info.Generation
	}
	// 如果nodeSnapshot中node的数量大于nodeTree中的数量，说明有node被删除
       // 所以要从快照的nodeInfoMap中删除已删除的Node，同时标记需要更新node的全量列
	// Comparing to pods in nodeTree.
	// Deleted nodes get removed from the tree, but they might remain in the nodes map
	// if they still have non-deleted Pods.
	if len(nodeSnapshot.nodeInfoMap) > cache.nodeTree.numNodes {
		cache.removeDeletedNodesFromSnapshot(nodeSnapshot)
		updateAllLists = true
	}
        // 如果需要更新Node的全量或者亲和性或者反亲和性列表，则更新nodeSnapshot中的Node列表
	if updateAllLists || updateNodesHavePodsWithAffinity || updateNodesHavePodsWithRequiredAntiAffinity {
		cache.updateNodeInfoSnapshotList(nodeSnapshot, updateAllLists)
	}
	// 如果此时nodeSnapshot的node列表与nodeTree的数量还不一致，需要再做一次node全列表更新
        // 此处应该是一个保险操作，理论上不会发生，谁知道会不会有Bug发生呢？多一些容错没有坏处
	if len(nodeSnapshot.nodeInfoList) != cache.nodeTree.numNodes {
		errMsg := fmt.Sprintf("snapshot state is not consistent, length of NodeInfoList=%v not equal to length of nodes in tree=%v "+
			", length of NodeInfoMap=%v, length of nodes in cache=%v"+
			", trying to recover",
			len(nodeSnapshot.nodeInfoList), cache.nodeTree.numNodes,
			len(nodeSnapshot.nodeInfoMap), len(cache.nodes))
		klog.ErrorS(nil, errMsg)
		// We will try to recover by re-creating the lists for the next scheduling cycle, but still return an
		// error to surface the problem, the error will likely cause a failure to the current scheduling cycle.
		cache.updateNodeInfoSnapshotList(nodeSnapshot, true)
		return fmt.Errorf(errMsg)
	}

	return nil
}

// 先思考一个问题：为什么有Node添加或者删除需要更新快照中的全量列表？如果是Node删除了，
// 需要找到Node在全量列表中的位置，然后删除它，最悲观的复杂度就是遍历一遍列表，然后再挪动它后面的Node
// 因为快照的Node列表是用slice实现，所以一旦快照中Node列表有任何更新，复杂度都是Node的数量。
// 那如果是有新的Node添加呢？并不知道应该插在哪里，所以重新创建一次全量列表最为简单有效。
// 亲和性和反亲和性列表道理也是一样的。
func (cache *schedulerCache) updateNodeInfoSnapshotList(snapshot *Snapshot, updateAll bool) {
	// 快照创建亲和性和反亲和性列表
	snapshot.havePodsWithAffinityNodeInfoList = make([]*framework.NodeInfo, 0, cache.nodeTree.numNodes)
	snapshot.havePodsWithRequiredAntiAffinityNodeInfoList = make([]*framework.NodeInfo, 0, cache.nodeTree.numNodes)
	// 如果更新全量列表
	if updateAll {
		// 创建快照全量列表
		// Take a snapshot of the nodes order in the tree
		snapshot.nodeInfoList = make([]*framework.NodeInfo, 0, cache.nodeTree.numNodes)
		nodesList, err := cache.nodeTree.list()
		if err != nil {
			klog.ErrorS(err, "Error occurred while retrieving the list of names of the nodes from node tree")
		}
		// 遍历nodeTree的Node
		for _, nodeName := range nodesList {
			// 理论上快照的nodeInfoMap与nodeTree的状态是一致，此处做了判断用来检测BUG，下面的错误日志也是这么写的
			if nodeInfo := snapshot.nodeInfoMap[nodeName]; nodeInfo != nil {
				// 追加全量、亲和性(按需)、反亲和性列表(按需)
				snapshot.nodeInfoList = append(snapshot.nodeInfoList, nodeInfo)
				if len(nodeInfo.PodsWithAffinity) > 0 {
					snapshot.havePodsWithAffinityNodeInfoList = append(snapshot.havePodsWithAffinityNodeInfoList, nodeInfo)
				}
				if len(nodeInfo.PodsWithRequiredAntiAffinity) > 0 {
					snapshot.havePodsWithRequiredAntiAffinityNodeInfoList = append(snapshot.havePodsWithRequiredAntiAffinityNodeInfoList, nodeInfo)
				}
			} else {
				klog.ErrorS(nil, "Node exists in nodeTree but not in NodeInfoMap, this should not happen", "node", klog.KRef("", nodeName))
			}
		}
	} else {
		// 如果更新全量列表，只需要遍历快照中的全量列表就可以了
		for _, nodeInfo := range snapshot.nodeInfoList {
			// 按需追加亲和性和反亲和性列表
			if len(nodeInfo.PodsWithAffinity) > 0 {
				snapshot.havePodsWithAffinityNodeInfoList = append(snapshot.havePodsWithAffinityNodeInfoList, nodeInfo)
			}
			if len(nodeInfo.PodsWithRequiredAntiAffinity) > 0 {
				snapshot.havePodsWithRequiredAntiAffinityNodeInfoList = append(snapshot.havePodsWithRequiredAntiAffinityNodeInfoList, nodeInfo)
			}
		}
	}
}

// If certain nodes were deleted after the last snapshot was taken, we should remove them from the snapshot.
func (cache *schedulerCache) removeDeletedNodesFromSnapshot(snapshot *Snapshot) {
	toDelete := len(snapshot.nodeInfoMap) - cache.nodeTree.numNodes
	for name := range snapshot.nodeInfoMap {
		if toDelete <= 0 {
			break
		}
		if n, ok := cache.nodes[name]; !ok || n.info.Node() == nil {
			delete(snapshot.nodeInfoMap, name)
			toDelete--
		}
	}
}

// NodeCount returns the number of nodes in the cache.
// DO NOT use outside of tests.
func (cache *schedulerCache) NodeCount() int {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	return len(cache.nodes)
}

// PodCount returns the number of pods in the cache (including those from deleted nodes).
// DO NOT use outside of tests.
func (cache *schedulerCache) PodCount() (int, error) {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	// podFilter is expected to return true for most or all of the pods. We
	// can avoid expensive array growth without wasting too much memory by
	// pre-allocating capacity.
	count := 0
	for _, n := range cache.nodes {
		count += len(n.info.Pods)
	}
	return count, nil
}
// 当kube-scheduler找到最优的Node调度Pod的时候会调用AssumePod假定Pod调度，在通过另一个协程异步Bind。假定其实就是预先占住资源，
// kube-scheduler调度下一个Pod的时候不会把这部分资源抢走，
// 直到收到确认消息AddPod确认调度成功，亦或是Bind失败ForgetPod取消假定调
func (cache *schedulerCache) AssumePod(pod *v1.Pod) error {
	key, err := framework.GetPodKey(pod)
	if err != nil {
		return err
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()
	// 如果Pod已经存在，则不能假定调度。因为在Cache中的Pod要么是假定调度的，要么是完成调度的
	if _, ok := cache.podStates[key]; ok {
		return fmt.Errorf("pod %v is in the cache, so can't be assumed", key)
	}
        // 添加pod,会判断是否是假定的pod
	return cache.addPod(pod, true)
}

func (cache *schedulerCache) FinishBinding(pod *v1.Pod) error {
	return cache.finishBinding(pod, time.Now())
}

// 当假定Pod绑定完成后,将缓存中绑定的标志位为true,同时会有一个协程去清理是否bing
// finishBinding exists to make tests determinitistic by injecting now as an argument
func (cache *schedulerCache) finishBinding(pod *v1.Pod, now time.Time) error {
	key, err := framework.GetPodKey(pod)
	if err != nil {
		return err
	}

	cache.mu.RLock()
	defer cache.mu.RUnlock()

	klog.V(5).InfoS("Finished binding for pod, can be expired", "pod", klog.KObj(pod))
	currState, ok := cache.podStates[key]
	if ok && cache.assumedPods.Has(key) {
		// 完成绑定后，设置假定的过期时间，如果在过期时间内没有得到Addpod的确认，就删除假定，并且把占用的节点资源清楚掉
		dl := now.Add(cache.ttl)
		currState.bindingFinished = true
		currState.deadline = &dl
	}
	return nil
}

// 假定Pod预先占用了一些资源，如果之后的操作(比如Bind)有什么错误，就需要取消假定调度，释放出资源。
func (cache *schedulerCache) ForgetPod(pod *v1.Pod) error {
	key, err := framework.GetPodKey(pod)
	if err != nil {
		return err
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()
	// 这里有意思了，也就是说Cache假定Pod的Node名字与传入的Pod的Node名字不一致，则返回错误
       // 这种情况会不会发生呢?有可能，但是可能性不大，毕竟多协程修改Pod调度状态会有各种可能性。
	currState, ok := cache.podStates[key]
	if ok && currState.pod.Spec.NodeName != pod.Spec.NodeName {
		return fmt.Errorf("pod %v was assumed on %v but assigned to %v", key, pod.Spec.NodeName, currState.pod.Spec.NodeName)
	}
	// 只有假定Pod可以被Forget，因为Forget就是为了取消假定Pod的。
	// Only assumed pod can be forgotten.
	if ok && cache.assumedPods.Has(key) {
		return cache.removePod(pod)
	}
	return fmt.Errorf("pod %v wasn't assumed so cannot be forgotten", key)
}

// Assumes that lock is already acquired.
func (cache *schedulerCache) addPod(pod *v1.Pod, assumePod bool) error {
	key, err := framework.GetPodKey(pod)
	if err != nil {
		return err
	}
	// 是否在所有节点项的集合中
	n, ok := cache.nodes[pod.Spec.NodeName]
	if !ok {
	   // 如果不在，为什么要新建一个呢？pod 自身已经定义了node name,但是这个node不在缓存中，可能kube-scheduler调度Pod的时候Node被删除了，可能很快还会添加回来
           // 也可能就彻底删除了，此时先放在这个虚的Node上，如果Node不存在后期还会被迁移。
		n = newNodeInfoListItem(framework.NewNodeInfo())
		cache.nodes[pod.Spec.NodeName] = n
	}
	// AddPod就是把Pod的资源累加到NodeInfo中，同时n.info.AddPod(pod)会更新NodeInfo.Generation
	n.info.AddPod(pod)
	cache.moveNodeInfoToHead(pod.Spec.NodeName)
	ps := &podState{
		pod: pod,
	}
	cache.podStates[key] = ps
	if assumePod {
		cache.assumedPods.Insert(key)
	}
	return nil
}

// 更新Pod，其实就是删除再添加，全量替换
// Assumes that lock is already acquired.
func (cache *schedulerCache) updatePod(oldPod, newPod *v1.Pod) error {
	if err := cache.removePod(oldPod); err != nil {
		return err
	}
	return cache.addPod(newPod, false)
}


// Assumes that lock is already acquired.
// Removes a pod from the cached node info. If the node information was already
// removed and there are no more pods left in the node, cleans up the node from
// the cache.
func (cache *schedulerCache) removePod(pod *v1.Pod) error {

	key, err := framework.GetPodKey(pod)
	if err != nil {
		return err
	}
       // 找到假定Pod调度的Node
	n, ok := cache.nodes[pod.Spec.NodeName]
	if !ok {
		klog.ErrorS(nil, "Node not found when trying to remove pod", "node", klog.KRef("", pod.Spec.NodeName), "pod", klog.KObj(pod))
	} else {
		// 减去假定Pod的资源，并从NodeInfo的Pod列表移除假定Pod
                // 和n.info.AddPod相同，也会更新NodeInfo.Generation
		if err := n.info.RemovePod(pod); err != nil {
			return err
		}
		// 如果NodeInfo的Pod列表没有任何Pod并且Node被删除，则Node从Cache中删除
                // 否则将NodeInfo移到列表头，因为NodeInfo被更新，需要放到表头
                // 这里需要知道的是，Node被删除Cache不会立刻删除该Node，需要等到Node上所有的Pod从Node中迁移后才删除，
		if len(n.info.Pods) == 0 && n.info.Node() == nil {
			cache.removeNodeInfoFromList(pod.Spec.NodeName)
		} else {
			cache.moveNodeInfoToHead(pod.Spec.NodeName)
		}
	}

	delete(cache.podStates, key)
	delete(cache.assumedPods, key)
	return nil
}
// 存在两种场景，第一 pod 假定完以后，同时调度成功了进行确认。 第二 调度服务重启后，加载已经被调度的pod进入缓存
func (cache *schedulerCache) AddPod(pod *v1.Pod) error {
	key, err := framework.GetPodKey(pod)
	if err != nil {
		return err
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()
        // 是否在缓存中存在
	currState, ok := cache.podStates[key]
	switch {
	// 如果在缓存中，且假定的缓存也存在
	case ok && cache.assumedPods.Has(key):
		if currState.pod.Spec.NodeName != pod.Spec.NodeName {
			// 以假定为准，更新缓存
			// The pod was added to a different node than it was assumed to.
			klog.InfoS("Pod was added to a different node than it was assumed", "pod", klog.KObj(pod), "assumedNode", klog.KRef("", pod.Spec.NodeName), "currentNode", klog.KRef("", currState.pod.Spec.NodeName))
			if err = cache.updatePod(currState.pod, pod); err != nil {
				klog.ErrorS(err, "Error occurred while updating pod")
			}
		} else {
			// 删除假定缓存（调度成功后的确认流程）
			delete(cache.assumedPods, key)
			cache.podStates[key].deadline = nil
			cache.podStates[key].pod = pod
		}
	case !ok:
		// 这里目前分为两种情况，第一种调度器发生重启，需要将已经被调度的，重新加入缓存
		// 第二种，可能会发生，当Bind完成，nodename被填充，但是pod 已经被调度的事件迟迟没有发出（大于30s）.所以这里把它重新添加回来
		// Pod was expired. We should add it back.
		if err = cache.addPod(pod, false); err != nil {
			klog.ErrorS(err, "Error occurred while adding pod")
		}
	default:
		return fmt.Errorf("pod %v was already in added state", key)
	}
	return nil
}

// 更新pod  先删除后添加
func (cache *schedulerCache) UpdatePod(oldPod, newPod *v1.Pod) error {
	key, err := framework.GetPodKey(oldPod)
	if err != nil {
		return err
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()

	currState, ok := cache.podStates[key]
	// An assumed pod won't have Update/Remove event. It needs to have Add event
	// before Update event, in which case the state would change from Assumed to Added.
	if ok && !cache.assumedPods.Has(key) {
		if currState.pod.Spec.NodeName != newPod.Spec.NodeName {
			klog.ErrorS(nil, "Pod updated on a different node than previously added to", "pod", klog.KObj(oldPod))
			klog.ErrorS(nil, "SchedulerCache is corrupted and can badly affect scheduling decisions")
			os.Exit(1)
		}
		return cache.updatePod(oldPod, newPod)
	}
	return fmt.Errorf("pod %v is not added to scheduler cache, so cannot be updated", key)
}

// kube-scheduler收到删除Pod的请求，如果Pod在Cache中，就需要调用RemovePod
func (cache *schedulerCache) RemovePod(pod *v1.Pod) error {
	key, err := framework.GetPodKey(pod)
	if err != nil {
		return err
	}

	cache.mu.Lock()
	defer cache.mu.Unlock()

	currState, ok := cache.podStates[key]
	if !ok {
		return fmt.Errorf("pod %v is not found in scheduler cache, so cannot be removed from it", key)
	}
	if currState.pod.Spec.NodeName != pod.Spec.NodeName {
		klog.ErrorS(nil, "Pod was added to a different node than it was assumed", "pod", klog.KObj(pod), "assumedNode", klog.KRef("", pod.Spec.NodeName), "currentNode", klog.KRef("", currState.pod.Spec.NodeName))
		if pod.Spec.NodeName != "" {
			// An empty NodeName is possible when the scheduler misses a Delete
			// event and it gets the last known state from the informer cache.
			klog.ErrorS(nil, "SchedulerCache is corrupted and can badly affect scheduling decisions")
			os.Exit(1)
		}
	}
	// 从NodeInfo中减去Pod的资源
	return cache.removePod(currState.pod)
}

// 判断pod是否处于假定
func (cache *schedulerCache) IsAssumedPod(pod *v1.Pod) (bool, error) {
	key, err := framework.GetPodKey(pod)
	if err != nil {
		return false, err
	}

	cache.mu.RLock()
	defer cache.mu.RUnlock()

	return cache.assumedPods.Has(key), nil
}

// 从缓存中获取pod信息
// GetPod might return a pod for which its node has already been deleted from
// the main cache. This is useful to properly process pod update events.
func (cache *schedulerCache) GetPod(pod *v1.Pod) (*v1.Pod, error) {
	key, err := framework.GetPodKey(pod)
	if err != nil {
		return nil, err
	}

	cache.mu.RLock()
	defer cache.mu.RUnlock()

	podState, ok := cache.podStates[key]
	if !ok {
		return nil, fmt.Errorf("pod %v does not exist in scheduler cache", key)
	}

	return podState.pod, nil
}

// 有新的Node添加到集群，kube-scheduler调用该接口通知Cache
func (cache *schedulerCache) AddNode(node *v1.Node) *framework.NodeInfo {
	cache.mu.Lock()
	defer cache.mu.Unlock()

	n, ok := cache.nodes[node.Name]
	if !ok {
		 // 如果NodeInfo不存在则创建
		n = newNodeInfoListItem(framework.NewNodeInfo())
		cache.nodes[node.Name] = n
	} else {
		// 已存在，先删除镜像状态，因为后面还会在添加回来
		cache.removeNodeImageStates(n.info.Node())
	}
	// 将Node放到列表头
	cache.moveNodeInfoToHead(node.Name)
        // 添加到nodeTree中
	cache.nodeTree.addNode(node)
	// 添加Node的镜像状态
	cache.addNodeImageStates(node, n.info)
	// 只有SetNode的NodeInfo才是真实的Node，否则就是前文提到的虚的Node
	n.info.SetNode(node)
	return n.info.Clone()
}
// 更新Node的全部信息
func (cache *schedulerCache) UpdateNode(oldNode, newNode *v1.Node) *framework.NodeInfo {
	cache.mu.Lock()
	defer cache.mu.Unlock()

	n, ok := cache.nodes[newNode.Name]
	if !ok {
		n = newNodeInfoListItem(framework.NewNodeInfo())
		cache.nodes[newNode.Name] = n
		cache.nodeTree.addNode(newNode)
	} else {
		cache.removeNodeImageStates(n.info.Node())
	}
	cache.moveNodeInfoToHead(newNode.Name)

	cache.nodeTree.updateNode(oldNode, newNode)
	cache.addNodeImageStates(newNode, n.info)
	n.info.SetNode(newNode)
	return n.info.Clone()
}

// Node从集群中删除，kube-scheduler调用该接口通知Cache（思考一个问题，当pod处于假定被确认时，这时候节点被删除，k8s 是怎么做的）
// RemoveNode removes a node from the cache's tree.
// The node might still have pods because their deletion events didn't arrive
// yet. Those pods are considered removed from the cache, being the node tree
// the source of truth.
// However, we keep a ghost node with the list of pods until all pod deletion
// events have arrived. A ghost node is skipped from snapshots.
func (cache *schedulerCache) RemoveNode(node *v1.Node) error {
	cache.mu.Lock()
	defer cache.mu.Unlock()

	n, ok := cache.nodes[node.Name]
	if !ok {
		return fmt.Errorf("node %v is not found", node.Name)
	}
	n.info.RemoveNode()
	// 当Node上没有运行Pod的时候删除Node，否则把Node放在列表头，因为Node状态更新了
        // 熟悉etcd的同学会知道，watch两个路径(Node和Pod)是两个通道，这样会造成两个通道的事件不会按照严格时序到达
        // 关联到pod的事件，两个事件有可能是并发到达的，那么对于node链表的操作，可能会发生问题，所以保持，更新节点链表的操作
	// 因为这个链表实际上只是记录更新的增量，真实的调度是以nodetree中的节点数量为准的，也不会影响调度。这应该是存在虚Node的原因之一。
	// We remove NodeInfo for this node only if there aren't any pods on this node.
	// We can't do it unconditionally, because notifications about pods are delivered
	// in a different watch, and thus can potentially be observed later, even though
	// they happened before node removal.
	if len(n.info.Pods) == 0 {
		// 当Node上没有运行Pod的时候删除Node
		cache.removeNodeInfoFromList(node.Name)
	} else {
		// 把Node放在列表头，因为Node状态更新了
		cache.moveNodeInfoToHead(node.Name)
	}
	// 虽然nodes只有在NodeInfo中Pod数量为零的时候才会被删除，但是nodeTree会直接删除
      // 说明nodeTree中体现了实际的Node状态，kube-scheduler调度Pod的时候也是利用nodeTree
      // 这样就不会将Pod调度到已经删除的Node上了
	if err := cache.nodeTree.removeNode(node); err != nil {
		return err
	}
	cache.removeNodeImageStates(node)
	return nil
}

// 添加节点的镜像信息
// addNodeImageStates adds states of the images on given node to the given nodeInfo and update the imageStates in
// scheduler cache. This function assumes the lock to scheduler cache has been acquired.
func (cache *schedulerCache) addNodeImageStates(node *v1.Node, nodeInfo *framework.NodeInfo) {
	newSum := make(map[string]*framework.ImageStateSummary)

	for _, image := range node.Status.Images {
		for _, name := range image.Names {
			// update the entry in imageStates
			state, ok := cache.imageStates[name]
			if !ok {
				state = &imageState{
					size:  image.SizeBytes,
					nodes: sets.NewString(node.Name),
				}
				cache.imageStates[name] = state
			} else {
				state.nodes.Insert(node.Name)
			}
			// create the imageStateSummary for this image
			if _, ok := newSum[name]; !ok {
				newSum[name] = cache.createImageStateSummary(state)
			}
		}
	}
	nodeInfo.ImageStates = newSum
}

// 移除节点的镜像状态信息
// removeNodeImageStates removes the given node record from image entries having the node
// in imageStates cache. After the removal, if any image becomes free, i.e., the image
// is no longer available on any node, the image entry will be removed from imageStates.
func (cache *schedulerCache) removeNodeImageStates(node *v1.Node) {
	if node == nil {
		return
	}

	for _, image := range node.Status.Images {
		for _, name := range image.Names {
			state, ok := cache.imageStates[name]
			if ok {
				state.nodes.Delete(node.Name)
				if len(state.nodes) == 0 {
					// Remove the unused image to make sure the length of
					// imageStates represents the total number of different
					// images on all nodes
					delete(cache.imageStates, name)
				}
			}
		}
	}
}

// 根据period周期定时清理
func (cache *schedulerCache) run() {
	go wait.Until(cache.cleanupExpiredAssumedPods, cache.period, cache.stop)
}

func (cache *schedulerCache) cleanupExpiredAssumedPods() {
	cache.cleanupAssumedPods(time.Now())
}

// 清理假定过期的Pod，已经被绑定，但是没被addpod 确认的。也就是没有迟迟没有收到调度成功的事件
// cleanupAssumedPods exists for making test deterministic by taking time as input argument.
// It also reports metrics on the cache size for nodes, pods, and assumed pods.
func (cache *schedulerCache) cleanupAssumedPods(now time.Time) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	defer cache.updateMetrics()
	
	// 遍历假定Pod
	// The size of assumedPods should be small
	for key := range cache.assumedPods {
		ps, ok := cache.podStates[key]
		if !ok {
			klog.ErrorS(nil, "Key found in assumed set but not in podStates, potentially a logical error")
			os.Exit(1)
		}
		// 如果Pod没有标记为结束Binding，则忽略，说明Pod还在Binding中
                // 说白了就是没有调用FinishBinding的Pod不用处理
		if !ps.bindingFinished {
			klog.V(5).InfoS("Could not expire cache for pod as binding is still in progress",
				"pod", klog.KObj(ps.pod))
			continue
		}
		// 如果当前时间已经超过了Pod假定过期时间，说明Pod假定时间已过期
		if now.After(*ps.deadline) {
			klog.InfoS("Pod expired", "pod", klog.KObj(ps.pod))
			// 清理假定过期的Pod
			if err := cache.removePod(ps.pod); err != nil {
				klog.ErrorS(err, "ExpirePod failed", "pod", klog.KObj(ps.pod))
			}
		}
	}
}

// updateMetrics updates cache size metric values for pods, assumed pods, and nodes
func (cache *schedulerCache) updateMetrics() {
	metrics.CacheSize.WithLabelValues("assumed_pods").Set(float64(len(cache.assumedPods)))
	metrics.CacheSize.WithLabelValues("pods").Set(float64(len(cache.podStates)))
	metrics.CacheSize.WithLabelValues("nodes").Set(float64(len(cache.nodes)))
}
