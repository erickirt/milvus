// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package observers

import (
	"context"
	"sync"
	"time"

	"github.com/samber/lo"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus/internal/coordinator/snmanager"
	"github.com/milvus-io/milvus/internal/querycoordv2/meta"
	"github.com/milvus-io/milvus/internal/querycoordv2/params"
	"github.com/milvus-io/milvus/internal/querycoordv2/utils"
	"github.com/milvus-io/milvus/internal/util/streamingutil"
	"github.com/milvus-io/milvus/pkg/v2/log"
	"github.com/milvus-io/milvus/pkg/v2/util/paramtable"
	"github.com/milvus-io/milvus/pkg/v2/util/syncutil"
	"github.com/milvus-io/milvus/pkg/v2/util/typeutil"
)

// check replica, find read only nodes and remove it from replica if all segment/channel has been moved
type ReplicaObserver struct {
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	meta      *meta.Meta
	distMgr   *meta.DistributionManager
	targetMgr meta.TargetManagerInterface

	startOnce sync.Once
	stopOnce  sync.Once
}

func NewReplicaObserver(meta *meta.Meta, distMgr *meta.DistributionManager) *ReplicaObserver {
	return &ReplicaObserver{
		meta:    meta,
		distMgr: distMgr,
	}
}

func (ob *ReplicaObserver) Start() {
	ob.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		ob.cancel = cancel

		ob.wg.Add(1)
		go ob.schedule(ctx)
		if streamingutil.IsStreamingServiceEnabled() {
			ob.wg.Add(1)
			go ob.scheduleStreamingQN(ctx)
		}
	})
}

func (ob *ReplicaObserver) Stop() {
	ob.stopOnce.Do(func() {
		if ob.cancel != nil {
			ob.cancel()
		}
		ob.wg.Wait()
	})
}

func (ob *ReplicaObserver) schedule(ctx context.Context) {
	defer ob.wg.Done()
	log.Info("Start check replica loop")

	listener := ob.meta.ResourceManager.ListenNodeChanged(ctx)
	for {
		ob.waitNodeChangedOrTimeout(ctx, listener)
		// stop if the context is canceled.
		if ctx.Err() != nil {
			log.Info("Stop check replica observer")
			return
		}

		// do check once.
		ob.checkNodesInReplica()
	}
}

// scheduleStreamingQN is used to check streaming query node in replica
func (ob *ReplicaObserver) scheduleStreamingQN(ctx context.Context) {
	defer ob.wg.Done()
	log.Info("Start streaming query node check replica loop")

	listener := snmanager.StaticStreamingNodeManager.ListenNodeChanged()
	for {
		ob.waitNodeChangedOrTimeout(ctx, listener)
		if ctx.Err() != nil {
			log.Info("Stop streaming query node check replica observer")
			return
		}

		ids := snmanager.StaticStreamingNodeManager.GetStreamingQueryNodeIDs()
		ob.checkStreamingQueryNodesInReplica(ids)
	}
}

func (ob *ReplicaObserver) waitNodeChangedOrTimeout(ctx context.Context, listener *syncutil.VersionedListener) {
	ctxWithTimeout, cancel := context.WithTimeout(ctx, params.Params.QueryCoordCfg.CheckNodeInReplicaInterval.GetAsDuration(time.Second))
	defer cancel()
	listener.Wait(ctxWithTimeout)
}

func (ob *ReplicaObserver) checkStreamingQueryNodesInReplica(sqNodeIDs typeutil.UniqueSet) {
	ctx := context.Background()
	log := log.Ctx(ctx).WithRateGroup("qcv2.checkStreamingQueryNodesInReplica", 1, 60)
	collections := ob.meta.GetAll(context.Background())

	for _, collectionID := range collections {
		ob.meta.RecoverSQNodesInCollection(context.Background(), collectionID, sqNodeIDs)
	}

	for _, collectionID := range collections {
		replicas := ob.meta.ReplicaManager.GetByCollection(ctx, collectionID)
		for _, replica := range replicas {
			roSQNodes := replica.GetROSQNodes()
			rwSQNodes := replica.GetRWSQNodes()
			if len(roSQNodes) == 0 {
				continue
			}
			removeNodes := make([]int64, 0, len(roSQNodes))
			for _, node := range roSQNodes {
				channels := ob.distMgr.ChannelDistManager.GetByCollectionAndFilter(replica.GetCollectionID(), meta.WithNodeID2Channel(node))
				segments := ob.distMgr.SegmentDistManager.GetByFilter(meta.WithCollectionID(collectionID), meta.WithNodeID(node))
				if len(channels) == 0 && len(segments) == 0 {
					removeNodes = append(removeNodes, node)
				}
			}
			if len(removeNodes) == 0 {
				continue
			}
			logger := log.With(
				zap.Int64("collectionID", replica.GetCollectionID()),
				zap.Int64("replicaID", replica.GetID()),
				zap.Int64s("removedNodes", removeNodes),
				zap.Int64s("roNodes", roSQNodes),
				zap.Int64s("rwNodes", rwSQNodes),
			)
			if err := ob.meta.ReplicaManager.RemoveSQNode(ctx, replica.GetID(), removeNodes...); err != nil {
				logger.Warn("fail to remove streaming query node from replica", zap.Error(err))
				continue
			}
			logger.Info("all segment/channel has been removed from ro streaming query node, remove it from replica")
		}
	}
}

func (ob *ReplicaObserver) checkNodesInReplica() {
	ctx := context.Background()
	log := log.Ctx(ctx).WithRateGroup("qcv2.checkNodesInReplica", 1, 60)
	collections := ob.meta.GetAll(ctx)
	for _, collectionID := range collections {
		utils.RecoverReplicaOfCollection(ctx, ob.meta, collectionID)
	}

	balancePolicy := paramtable.Get().QueryCoordCfg.Balancer.GetValue()
	enableChannelExclusiveMode := balancePolicy == meta.ChannelLevelScoreBalancerName

	// check all ro nodes, remove it from replica if all segment/channel has been moved
	for _, collectionID := range collections {
		replicas := ob.meta.ReplicaManager.GetByCollection(ctx, collectionID)
		for _, replica := range replicas {
			if enableChannelExclusiveMode && !replica.IsChannelExclusiveModeEnabled() {
				// register channel for enable exclusive mode
				mutableReplica := replica.CopyForWrite()
				channels := ob.targetMgr.GetDmChannelsByCollection(ctx, collectionID, meta.CurrentTargetFirst)
				mutableReplica.TryEnableChannelExclusiveMode(lo.Keys(channels)...)
				replica = mutableReplica.IntoReplica()
				ob.meta.ReplicaManager.Put(ctx, replica)
			}

			roNodes := replica.GetRONodes()
			rwNodes := replica.GetRWNodes()
			if len(roNodes) == 0 {
				continue
			}
			logger := log.With(
				zap.Int64("collectionID", replica.GetCollectionID()),
				zap.Int64("replicaID", replica.GetID()),
				zap.Int64s("roNodes", roNodes),
				zap.Int64s("rwNodes", rwNodes),
			)

			log.RatedInfo(10, "found ro nodes in replica")
			removeNodes := make([]int64, 0, len(roNodes))
			for _, node := range roNodes {
				channels := ob.distMgr.ChannelDistManager.GetByCollectionAndFilter(replica.GetCollectionID(), meta.WithNodeID2Channel(node))
				segments := ob.distMgr.SegmentDistManager.GetByFilter(meta.WithCollectionID(collectionID), meta.WithNodeID(node))
				if len(channels) == 0 && len(segments) == 0 {
					removeNodes = append(removeNodes, node)
				}
			}
			if len(removeNodes) == 0 {
				continue
			}
			if err := ob.meta.ReplicaManager.RemoveNode(ctx, replica.GetID(), removeNodes...); err != nil {
				logger.Warn("fail to remove node from replica",
					zap.Int64s("removedNodes", removeNodes),
					zap.Error(err))
				continue
			}
			logger.Info("all segment/channel has been removed from ro node, remove it from replica",
				zap.Int64s("removedNodes", removeNodes),
			)
		}
	}
}
