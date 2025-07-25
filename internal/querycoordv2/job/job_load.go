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

package job

import (
	"context"
	"fmt"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/samber/lo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus-proto/go-api/v2/milvuspb"
	"github.com/milvus-io/milvus/internal/querycoordv2/meta"
	"github.com/milvus-io/milvus/internal/querycoordv2/observers"
	"github.com/milvus-io/milvus/internal/querycoordv2/session"
	"github.com/milvus-io/milvus/internal/querycoordv2/utils"
	"github.com/milvus-io/milvus/pkg/v2/eventlog"
	"github.com/milvus-io/milvus/pkg/v2/log"
	"github.com/milvus-io/milvus/pkg/v2/metrics"
	"github.com/milvus-io/milvus/pkg/v2/proto/querypb"
	"github.com/milvus-io/milvus/pkg/v2/util/merr"
	"github.com/milvus-io/milvus/pkg/v2/util/typeutil"
)

type LoadCollectionJob struct {
	*BaseJob
	req  *querypb.LoadCollectionRequest
	undo *UndoList

	dist                     *meta.DistributionManager
	meta                     *meta.Meta
	broker                   meta.Broker
	targetMgr                meta.TargetManagerInterface
	targetObserver           *observers.TargetObserver
	collectionObserver       *observers.CollectionObserver
	nodeMgr                  *session.NodeManager
	collInfo                 *milvuspb.DescribeCollectionResponse
	userSpecifiedReplicaMode bool
}

func NewLoadCollectionJob(
	ctx context.Context,
	req *querypb.LoadCollectionRequest,
	dist *meta.DistributionManager,
	meta *meta.Meta,
	broker meta.Broker,
	targetMgr meta.TargetManagerInterface,
	targetObserver *observers.TargetObserver,
	collectionObserver *observers.CollectionObserver,
	nodeMgr *session.NodeManager,
	userSpecifiedReplicaMode bool,
) *LoadCollectionJob {
	return &LoadCollectionJob{
		BaseJob:                  NewBaseJob(ctx, req.Base.GetMsgID(), req.GetCollectionID()),
		req:                      req,
		undo:                     NewUndoList(ctx, meta, targetMgr, targetObserver),
		dist:                     dist,
		meta:                     meta,
		broker:                   broker,
		targetMgr:                targetMgr,
		targetObserver:           targetObserver,
		collectionObserver:       collectionObserver,
		nodeMgr:                  nodeMgr,
		userSpecifiedReplicaMode: userSpecifiedReplicaMode,
	}
}

func (job *LoadCollectionJob) PreExecute() error {
	req := job.req
	log := log.Ctx(job.ctx).With(zap.Int64("collectionID", req.GetCollectionID()))

	if req.GetReplicaNumber() <= 0 {
		log.Info("request doesn't indicate the number of replicas, set it to 1",
			zap.Int32("replicaNumber", req.GetReplicaNumber()))
		req.ReplicaNumber = 1
	}

	if len(req.GetResourceGroups()) == 0 {
		req.ResourceGroups = []string{meta.DefaultResourceGroupName}
	}

	var err error
	job.collInfo, err = job.broker.DescribeCollection(job.ctx, req.GetCollectionID())
	if err != nil {
		log.Warn("failed to describe collection from RootCoord", zap.Error(err))
		return err
	}

	collection := job.meta.GetCollection(job.ctx, req.GetCollectionID())
	if collection == nil {
		return nil
	}

	if collection.GetReplicaNumber() != req.GetReplicaNumber() {
		msg := fmt.Sprintf("collection with different replica number %d existed, release this collection first before changing its replica number",
			job.meta.GetReplicaNumber(job.ctx, req.GetCollectionID()),
		)
		log.Warn(msg)
		return merr.WrapErrParameterInvalid(collection.GetReplicaNumber(), req.GetReplicaNumber(), "can't change the replica number for loaded collection")
	}

	collectionUsedRG := job.meta.ReplicaManager.GetResourceGroupByCollection(job.ctx, collection.GetCollectionID()).Collect()
	left, right := lo.Difference(collectionUsedRG, req.GetResourceGroups())
	if len(left) > 0 || len(right) > 0 {
		msg := fmt.Sprintf("collection with different resource groups %v existed, release this collection first before changing its resource groups",
			collectionUsedRG)
		log.Warn(msg)
		return merr.WrapErrParameterInvalid(collectionUsedRG, req.GetResourceGroups(), "can't change the resource groups for loaded partitions")
	}

	return nil
}

func (job *LoadCollectionJob) Execute() error {
	req := job.req
	log := log.Ctx(job.ctx).With(zap.Int64("collectionID", req.GetCollectionID()))
	meta.GlobalFailedLoadCache.Remove(req.GetCollectionID())

	// 1. Fetch target partitions
	partitionIDs, err := job.broker.GetPartitions(job.ctx, req.GetCollectionID())
	if err != nil {
		msg := "failed to get partitions from RootCoord"
		log.Warn(msg, zap.Error(err))
		return errors.Wrap(err, msg)
	}
	loadedPartitionIDs := lo.Map(job.meta.CollectionManager.GetPartitionsByCollection(job.ctx, req.GetCollectionID()),
		func(partition *meta.Partition, _ int) int64 {
			return partition.GetPartitionID()
		})
	lackPartitionIDs := lo.FilterMap(partitionIDs, func(partID int64, _ int) (int64, bool) {
		return partID, !lo.Contains(loadedPartitionIDs, partID)
	})
	if len(lackPartitionIDs) == 0 {
		return nil
	}
	job.undo.CollectionID = req.GetCollectionID()
	job.undo.LackPartitions = lackPartitionIDs
	log.Info("find partitions to load", zap.Int64s("partitions", lackPartitionIDs))

	colExisted := job.meta.CollectionManager.Exist(job.ctx, req.GetCollectionID())
	if !colExisted {
		// Clear stale replicas, https://github.com/milvus-io/milvus/issues/20444
		err = job.meta.ReplicaManager.RemoveCollection(job.ctx, req.GetCollectionID())
		if err != nil {
			msg := "failed to clear stale replicas"
			log.Warn(msg, zap.Error(err))
			return errors.Wrap(err, msg)
		}
	}

	// 2. create replica if not exist
	replicas := job.meta.ReplicaManager.GetByCollection(job.ctx, req.GetCollectionID())
	if len(replicas) == 0 {
		// API of LoadCollection is wired, we should use map[resourceGroupNames]replicaNumber as input, to keep consistency with `TransferReplica` API.
		// Then we can implement dynamic replica changed in different resource group independently.
		_, err = utils.SpawnReplicasWithRG(job.ctx, job.meta, req.GetCollectionID(), req.GetResourceGroups(),
			req.GetReplicaNumber(), job.collInfo.GetVirtualChannelNames(), req.GetPriority())
		if err != nil {
			msg := "failed to spawn replica for collection"
			log.Warn(msg, zap.Error(err))
			return errors.Wrap(err, msg)
		}
		job.undo.IsReplicaCreated = true
	}

	// 4. put collection/partitions meta
	partitions := lo.Map(lackPartitionIDs, func(partID int64, _ int) *meta.Partition {
		return &meta.Partition{
			PartitionLoadInfo: &querypb.PartitionLoadInfo{
				CollectionID:  req.GetCollectionID(),
				PartitionID:   partID,
				ReplicaNumber: req.GetReplicaNumber(),
				Status:        querypb.LoadStatus_Loading,
				FieldIndexID:  req.GetFieldIndexID(),
			},
			CreatedAt: time.Now(),
		}
	})

	ctx, sp := otel.Tracer(typeutil.QueryCoordRole).Start(job.ctx, "LoadCollection", trace.WithNewRoot())
	collection := &meta.Collection{
		CollectionLoadInfo: &querypb.CollectionLoadInfo{
			CollectionID:             req.GetCollectionID(),
			ReplicaNumber:            req.GetReplicaNumber(),
			Status:                   querypb.LoadStatus_Loading,
			FieldIndexID:             req.GetFieldIndexID(),
			LoadType:                 querypb.LoadType_LoadCollection,
			LoadFields:               req.GetLoadFields(),
			DbID:                     job.collInfo.GetDbId(),
			UserSpecifiedReplicaMode: job.userSpecifiedReplicaMode,
		},
		CreatedAt: time.Now(),
		LoadSpan:  sp,
		Schema:    job.collInfo.GetSchema(),
	}
	job.undo.IsNewCollection = true
	err = job.meta.CollectionManager.PutCollection(job.ctx, collection, partitions...)
	if err != nil {
		msg := "failed to store collection and partitions"
		log.Warn(msg, zap.Error(err))
		return errors.Wrap(err, msg)
	}
	eventlog.Record(eventlog.NewRawEvt(eventlog.Level_Info, fmt.Sprintf("Start load collection %d", collection.CollectionID)))
	metrics.QueryCoordNumPartitions.WithLabelValues().Add(float64(len(partitions)))

	// 5. update next target, no need to rollback if pull target failed, target observer will pull target in periodically
	_, err = job.targetObserver.UpdateNextTarget(req.GetCollectionID())
	if err != nil {
		msg := "failed to update next target"
		log.Warn(msg, zap.Error(err))
	}
	job.undo.IsTargetUpdated = true

	// 6. register load task into collection observer
	job.collectionObserver.LoadCollection(ctx, req.GetCollectionID())

	return nil
}

func (job *LoadCollectionJob) PostExecute() {
	if job.Error() != nil {
		job.undo.RollBack()
	}
}

type LoadPartitionJob struct {
	*BaseJob
	req  *querypb.LoadPartitionsRequest
	undo *UndoList

	dist                     *meta.DistributionManager
	meta                     *meta.Meta
	broker                   meta.Broker
	targetMgr                meta.TargetManagerInterface
	targetObserver           *observers.TargetObserver
	collectionObserver       *observers.CollectionObserver
	nodeMgr                  *session.NodeManager
	collInfo                 *milvuspb.DescribeCollectionResponse
	userSpecifiedReplicaMode bool
}

func NewLoadPartitionJob(
	ctx context.Context,
	req *querypb.LoadPartitionsRequest,
	dist *meta.DistributionManager,
	meta *meta.Meta,
	broker meta.Broker,
	targetMgr meta.TargetManagerInterface,
	targetObserver *observers.TargetObserver,
	collectionObserver *observers.CollectionObserver,
	nodeMgr *session.NodeManager,
	userSpecifiedReplicaMode bool,
) *LoadPartitionJob {
	return &LoadPartitionJob{
		BaseJob:                  NewBaseJob(ctx, req.Base.GetMsgID(), req.GetCollectionID()),
		req:                      req,
		undo:                     NewUndoList(ctx, meta, targetMgr, targetObserver),
		dist:                     dist,
		meta:                     meta,
		broker:                   broker,
		targetMgr:                targetMgr,
		targetObserver:           targetObserver,
		collectionObserver:       collectionObserver,
		nodeMgr:                  nodeMgr,
		userSpecifiedReplicaMode: userSpecifiedReplicaMode,
	}
}

func (job *LoadPartitionJob) PreExecute() error {
	req := job.req
	log := log.Ctx(job.ctx).With(zap.Int64("collectionID", req.GetCollectionID()))

	if req.GetReplicaNumber() <= 0 {
		log.Info("request doesn't indicate the number of replicas, set it to 1",
			zap.Int32("replicaNumber", req.GetReplicaNumber()))
		req.ReplicaNumber = 1
	}

	if len(req.GetResourceGroups()) == 0 {
		req.ResourceGroups = []string{meta.DefaultResourceGroupName}
	}

	var err error
	job.collInfo, err = job.broker.DescribeCollection(job.ctx, req.GetCollectionID())
	if err != nil {
		log.Warn("failed to describe collection from RootCoord", zap.Error(err))
		return err
	}

	collection := job.meta.GetCollection(job.ctx, req.GetCollectionID())
	if collection == nil {
		return nil
	}

	if collection.GetReplicaNumber() != req.GetReplicaNumber() {
		msg := "collection with different replica number existed, release this collection first before changing its replica number"
		log.Warn(msg)
		return merr.WrapErrParameterInvalid(collection.GetReplicaNumber(), req.GetReplicaNumber(), "can't change the replica number for loaded partitions")
	}

	collectionUsedRG := job.meta.ReplicaManager.GetResourceGroupByCollection(job.ctx, collection.GetCollectionID()).Collect()
	left, right := lo.Difference(collectionUsedRG, req.GetResourceGroups())
	if len(left) > 0 || len(right) > 0 {
		msg := fmt.Sprintf("collection with different resource groups %v existed, release this collection first before changing its resource groups",
			collectionUsedRG)
		log.Warn(msg)
		return merr.WrapErrParameterInvalid(collectionUsedRG, req.GetResourceGroups(), "can't change the resource groups for loaded partitions")
	}

	return nil
}

func (job *LoadPartitionJob) Execute() error {
	req := job.req
	log := log.Ctx(job.ctx).With(
		zap.Int64("collectionID", req.GetCollectionID()),
		zap.Int64s("partitionIDs", req.GetPartitionIDs()),
	)
	meta.GlobalFailedLoadCache.Remove(req.GetCollectionID())

	// 1. Fetch target partitions
	loadedPartitionIDs := lo.Map(job.meta.CollectionManager.GetPartitionsByCollection(job.ctx, req.GetCollectionID()),
		func(partition *meta.Partition, _ int) int64 {
			return partition.GetPartitionID()
		})
	lackPartitionIDs := lo.FilterMap(req.GetPartitionIDs(), func(partID int64, _ int) (int64, bool) {
		return partID, !lo.Contains(loadedPartitionIDs, partID)
	})
	if len(lackPartitionIDs) == 0 {
		return nil
	}
	job.undo.CollectionID = req.GetCollectionID()
	job.undo.LackPartitions = lackPartitionIDs
	log.Info("find partitions to load", zap.Int64s("partitions", lackPartitionIDs))

	var err error
	if !job.meta.CollectionManager.Exist(job.ctx, req.GetCollectionID()) {
		// Clear stale replicas, https://github.com/milvus-io/milvus/issues/20444
		err = job.meta.ReplicaManager.RemoveCollection(job.ctx, req.GetCollectionID())
		if err != nil {
			msg := "failed to clear stale replicas"
			log.Warn(msg, zap.Error(err))
			return errors.Wrap(err, msg)
		}
	}

	// 2. create replica if not exist
	replicas := job.meta.ReplicaManager.GetByCollection(context.TODO(), req.GetCollectionID())
	if len(replicas) == 0 {
		_, err = utils.SpawnReplicasWithRG(job.ctx, job.meta, req.GetCollectionID(), req.GetResourceGroups(), req.GetReplicaNumber(),
			job.collInfo.GetVirtualChannelNames(), req.GetPriority())
		if err != nil {
			msg := "failed to spawn replica for collection"
			log.Warn(msg, zap.Error(err))
			return errors.Wrap(err, msg)
		}
		job.undo.IsReplicaCreated = true
	}

	// 4. put collection/partitions meta
	partitions := lo.Map(lackPartitionIDs, func(partID int64, _ int) *meta.Partition {
		return &meta.Partition{
			PartitionLoadInfo: &querypb.PartitionLoadInfo{
				CollectionID:  req.GetCollectionID(),
				PartitionID:   partID,
				ReplicaNumber: req.GetReplicaNumber(),
				Status:        querypb.LoadStatus_Loading,
				FieldIndexID:  req.GetFieldIndexID(),
			},
			CreatedAt: time.Now(),
		}
	})
	ctx, sp := otel.Tracer(typeutil.QueryCoordRole).Start(job.ctx, "LoadPartition", trace.WithNewRoot())
	if !job.meta.CollectionManager.Exist(job.ctx, req.GetCollectionID()) {
		job.undo.IsNewCollection = true

		collection := &meta.Collection{
			CollectionLoadInfo: &querypb.CollectionLoadInfo{
				CollectionID:             req.GetCollectionID(),
				ReplicaNumber:            req.GetReplicaNumber(),
				Status:                   querypb.LoadStatus_Loading,
				FieldIndexID:             req.GetFieldIndexID(),
				LoadType:                 querypb.LoadType_LoadPartition,
				LoadFields:               req.GetLoadFields(),
				DbID:                     job.collInfo.GetDbId(),
				UserSpecifiedReplicaMode: job.userSpecifiedReplicaMode,
			},
			CreatedAt: time.Now(),
			LoadSpan:  sp,
			Schema:    job.collInfo.GetSchema(),
		}
		err = job.meta.CollectionManager.PutCollection(job.ctx, collection, partitions...)
		if err != nil {
			msg := "failed to store collection and partitions"
			log.Warn(msg, zap.Error(err))
			return errors.Wrap(err, msg)
		}
	} else { // collection exists, put partitions only
		coll := job.meta.GetCollection(job.ctx, req.GetCollectionID())
		if job.userSpecifiedReplicaMode && !coll.CollectionLoadInfo.UserSpecifiedReplicaMode {
			coll.CollectionLoadInfo.UserSpecifiedReplicaMode = job.userSpecifiedReplicaMode
			err = job.meta.CollectionManager.PutCollection(job.ctx, coll)
			if err != nil {
				msg := "failed to store collection"
				log.Warn(msg, zap.Error(err))
				return errors.Wrap(err, msg)
			}
		}

		err = job.meta.CollectionManager.PutPartition(job.ctx, partitions...)
		if err != nil {
			msg := "failed to store partitions"
			log.Warn(msg, zap.Error(err))
			return errors.Wrap(err, msg)
		}
	}
	metrics.QueryCoordNumPartitions.WithLabelValues().Add(float64(len(partitions)))

	// 5. update next target, no need to rollback if pull target failed, target observer will pull target in periodically
	_, err = job.targetObserver.UpdateNextTarget(req.GetCollectionID())
	if err != nil {
		msg := "failed to update next target"
		log.Warn(msg, zap.Error(err))
	}
	job.undo.IsTargetUpdated = true

	job.collectionObserver.LoadPartitions(ctx, req.GetCollectionID(), lackPartitionIDs)

	return nil
}

func (job *LoadPartitionJob) PostExecute() {
	if job.Error() != nil {
		job.undo.RollBack()
	}
}
