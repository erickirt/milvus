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

package datacoord

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/samber/lo"
	"go.uber.org/zap"

	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus-proto/go-api/v2/msgpb"
	"github.com/milvus-io/milvus/internal/datacoord/allocator"
	"github.com/milvus-io/milvus/pkg/v2/log"
	"github.com/milvus-io/milvus/pkg/v2/proto/datapb"
	"github.com/milvus-io/milvus/pkg/v2/util/lifetime"
	"github.com/milvus-io/milvus/pkg/v2/util/lock"
	"github.com/milvus-io/milvus/pkg/v2/util/logutil"
	"github.com/milvus-io/milvus/pkg/v2/util/paramtable"
	"github.com/milvus-io/milvus/pkg/v2/util/tsoutil"
	"github.com/milvus-io/milvus/pkg/v2/util/typeutil"
)

type compactTime struct {
	startTime     Timestamp
	expireTime    Timestamp
	collectionTTL time.Duration
}

// todo: migrate to compaction_trigger_v2
type trigger interface {
	start()
	stop()
	// triggerSingleCompaction triggers a compaction bundled with collection-partition-channel-segment
	triggerSingleCompaction(collectionID, partitionID, segmentID int64, channel string, blockToSendSignal bool) error
	// triggerManualCompaction force to start a compaction
	triggerManualCompaction(collectionID int64) (UniqueID, error)
}

type compactionSignal struct {
	id           UniqueID
	isForce      bool
	isGlobal     bool
	collectionID UniqueID
	partitionID  UniqueID
	channel      string
	segmentID    UniqueID
	pos          *msgpb.MsgPosition
}

var _ trigger = (*compactionTrigger)(nil)

type compactionTrigger struct {
	handler           Handler
	meta              *meta
	allocator         allocator.Allocator
	signals           chan *compactionSignal
	compactionHandler compactionPlanContext
	globalTrigger     *time.Ticker
	forceMu           lock.Mutex
	closeCh           lifetime.SafeChan
	closeWaiter       sync.WaitGroup

	indexEngineVersionManager IndexEngineVersionManager

	estimateNonDiskSegmentPolicy calUpperLimitPolicy
	estimateDiskSegmentPolicy    calUpperLimitPolicy
	// A sloopy hack, so we can test with different segment row count without worrying that
	// they are re-calculated in every compaction.
	testingOnly bool
}

func newCompactionTrigger(
	meta *meta,
	compactionHandler compactionPlanContext,
	allocator allocator.Allocator,
	handler Handler,
	indexVersionManager IndexEngineVersionManager,
) *compactionTrigger {
	return &compactionTrigger{
		meta:                         meta,
		allocator:                    allocator,
		signals:                      make(chan *compactionSignal, 100),
		compactionHandler:            compactionHandler,
		indexEngineVersionManager:    indexVersionManager,
		estimateDiskSegmentPolicy:    calBySchemaPolicyWithDiskIndex,
		estimateNonDiskSegmentPolicy: calBySchemaPolicy,
		handler:                      handler,
		closeCh:                      lifetime.NewSafeChan(),
	}
}

func (t *compactionTrigger) start() {
	t.globalTrigger = time.NewTicker(Params.DataCoordCfg.MixCompactionTriggerInterval.GetAsDuration(time.Second))
	t.closeWaiter.Add(2)
	go func() {
		defer logutil.LogPanic()
		defer t.closeWaiter.Done()

		for {
			select {
			case <-t.closeCh.CloseCh():
				log.Info("compaction trigger quit")
				return
			case signal := <-t.signals:
				switch {
				case signal.isGlobal:
					// ManualCompaction also use use handleGlobalSignal
					// so throw err here
					err := t.handleGlobalSignal(signal)
					if err != nil {
						log.Warn("unable to handleGlobalSignal", zap.Error(err))
					}
				default:
					// no need to handle err in handleSignal
					t.handleSignal(signal)
				}
			}
		}
	}()

	go t.startGlobalCompactionLoop()
}

func (t *compactionTrigger) startGlobalCompactionLoop() {
	defer logutil.LogPanic()
	defer t.closeWaiter.Done()

	// If AutoCompaction disabled, global loop will not start
	if !Params.DataCoordCfg.EnableAutoCompaction.GetAsBool() {
		return
	}

	for {
		select {
		case <-t.closeCh.CloseCh():
			t.globalTrigger.Stop()
			log.Info("global compaction loop exit")
			return
		case <-t.globalTrigger.C:
			err := t.triggerCompaction()
			if err != nil {
				log.Warn("unable to triggerCompaction", zap.Error(err))
			}
		}
	}
}

func (t *compactionTrigger) stop() {
	t.closeCh.Close()
	t.closeWaiter.Wait()
}

func (t *compactionTrigger) getCollection(collectionID UniqueID) (*collectionInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	coll, err := t.handler.GetCollection(ctx, collectionID)
	if err != nil {
		return nil, fmt.Errorf("collection ID %d not found, err: %w", collectionID, err)
	}
	return coll, nil
}

func isCollectionAutoCompactionEnabled(coll *collectionInfo) bool {
	enabled, err := getCollectionAutoCompactionEnabled(coll.Properties)
	if err != nil {
		log.Warn("collection properties auto compaction not valid, returning false", zap.Error(err))
		return false
	}
	return enabled
}

func getCompactTime(ts Timestamp, coll *collectionInfo) (*compactTime, error) {
	collectionTTL, err := getCollectionTTL(coll.Properties)
	if err != nil {
		return nil, err
	}

	pts, _ := tsoutil.ParseTS(ts)

	if collectionTTL > 0 {
		ttexpired := pts.Add(-collectionTTL)
		ttexpiredLogic := tsoutil.ComposeTS(ttexpired.UnixNano()/int64(time.Millisecond), 0)
		return &compactTime{ts, ttexpiredLogic, collectionTTL}, nil
	}

	// no expiration time
	return &compactTime{ts, 0, 0}, nil
}

// triggerCompaction trigger a compaction if any compaction condition satisfy.
func (t *compactionTrigger) triggerCompaction() error {
	id, err := t.allocSignalID()
	if err != nil {
		return err
	}
	signal := &compactionSignal{
		id:       id,
		isForce:  false,
		isGlobal: true,
	}
	t.signals <- signal
	return nil
}

// triggerSingleCompaction trigger a compaction bundled with collection-partition-channel-segment
func (t *compactionTrigger) triggerSingleCompaction(collectionID, partitionID, segmentID int64, channel string, blockToSendSignal bool) error {
	// If AutoCompaction disabled, flush request will not trigger compaction
	if !paramtable.Get().DataCoordCfg.EnableAutoCompaction.GetAsBool() && !paramtable.Get().DataCoordCfg.EnableCompaction.GetAsBool() {
		return nil
	}

	id, err := t.allocSignalID()
	if err != nil {
		return err
	}
	signal := &compactionSignal{
		id:           id,
		isForce:      false,
		isGlobal:     false,
		collectionID: collectionID,
		partitionID:  partitionID,
		segmentID:    segmentID,
		channel:      channel,
	}
	if blockToSendSignal {
		t.signals <- signal
		return nil
	}
	select {
	case t.signals <- signal:
	default:
		log.Info("no space to send compaction signal", zap.Int64("collectionID", collectionID), zap.Int64("segmentID", segmentID), zap.String("channel", channel))
	}

	return nil
}

// triggerManualCompaction force to start a compaction
// invoked by user `ManualCompaction` operation
func (t *compactionTrigger) triggerManualCompaction(collectionID int64) (UniqueID, error) {
	id, err := t.allocSignalID()
	if err != nil {
		return -1, err
	}
	signal := &compactionSignal{
		id:           id,
		isForce:      true,
		isGlobal:     true,
		collectionID: collectionID,
	}

	err = t.handleGlobalSignal(signal)
	if err != nil {
		log.Warn("unable to handle compaction signal", zap.Error(err))
		return -1, err
	}

	return id, nil
}

func (t *compactionTrigger) allocSignalID() (UniqueID, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return t.allocator.AllocID(ctx)
}

func (t *compactionTrigger) handleGlobalSignal(signal *compactionSignal) error {
	t.forceMu.Lock()
	defer t.forceMu.Unlock()

	log := log.With(zap.Int64("compactionID", signal.id),
		zap.Int64("signal.collectionID", signal.collectionID),
		zap.Int64("signal.partitionID", signal.partitionID),
		zap.Int64("signal.segmentID", signal.segmentID))
	filter := SegmentFilterFunc(func(segment *SegmentInfo) bool {
		return isSegmentHealthy(segment) &&
			isFlush(segment) &&
			!segment.isCompacting && // not compacting now
			!segment.GetIsImporting() && // not importing now
			segment.GetLevel() != datapb.SegmentLevel_L0 && // ignore level zero segments
			segment.GetLevel() != datapb.SegmentLevel_L2 && // ignore l2 segment
			!segment.GetIsInvisible()
	}) // partSegments is list of chanPartSegments, which is channel-partition organized segments

	partSegments := make([]*chanPartSegments, 0)
	// get all segments if signal.collection == 0, otherwise get collection segments
	if signal.collectionID != 0 {
		partSegments = GetSegmentsChanPart(t.meta, signal.collectionID, filter)
	} else {
		collections := t.meta.GetCollections()
		for _, collection := range collections {
			partSegments = append(partSegments, GetSegmentsChanPart(t.meta, collection.ID, filter)...)
		}
	}

	if len(partSegments) == 0 {
		log.Info("the length of SegmentsChanPart is 0, skip to handle compaction")
		return nil
	}

	for _, group := range partSegments {
		log := log.With(zap.Int64("collectionID", group.collectionID),
			zap.Int64("partitionID", group.partitionID),
			zap.String("channel", group.channelName))
		if !signal.isForce && t.compactionHandler.isFull() {
			log.Warn("compaction plan skipped due to handler full")
			break
		}

		if Params.DataCoordCfg.IndexBasedCompaction.GetAsBool() {
			group.segments = FilterInIndexedSegments(t.handler, t.meta, signal.isForce, group.segments...)
		}

		coll, err := t.getCollection(group.collectionID)
		if err != nil {
			log.Warn("get collection info failed, skip handling compaction", zap.Error(err))
			return err
		}

		if !signal.isForce && !isCollectionAutoCompactionEnabled(coll) {
			log.RatedInfo(20, "collection auto compaction disabled")
			return nil
		}

		ct, err := getCompactTime(tsoutil.ComposeTSByTime(time.Now(), 0), coll)
		if err != nil {
			log.Warn("get compact time failed, skip to handle compaction")
			return err
		}

		expectedSize := getExpectedSegmentSize(t.meta, coll)
		plans := t.generatePlans(group.segments, signal, ct, expectedSize)
		for _, plan := range plans {
			if !signal.isForce && t.compactionHandler.isFull() {
				log.Warn("compaction plan skipped due to handler full")
				break
			}
			totalRows, inputSegmentIDs := plan.A, plan.B

			// TODO[GOOSE], 11 = 1 planID + 10 segmentID, this is a hack need to be removed.
			// Any plan that output segment number greater than 10 will be marked as invalid plan for now.
			startID, endID, err := t.allocator.AllocN(11)
			if err != nil {
				log.Warn("fail to allocate id", zap.Error(err))
				return err
			}
			start := time.Now()
			pts, _ := tsoutil.ParseTS(ct.startTime)
			task := &datapb.CompactionTask{
				PlanID:           startID,
				TriggerID:        signal.id,
				State:            datapb.CompactionTaskState_pipelining,
				StartTime:        pts.Unix(),
				TimeoutInSeconds: Params.DataCoordCfg.CompactionTimeoutInSeconds.GetAsInt32(),
				Type:             datapb.CompactionType_MixCompaction,
				CollectionTtl:    ct.collectionTTL.Nanoseconds(),
				CollectionID:     group.collectionID,
				PartitionID:      group.partitionID,
				Channel:          group.channelName,
				InputSegments:    inputSegmentIDs,
				ResultSegments:   []int64{},
				TotalRows:        totalRows,
				Schema:           coll.Schema,
				MaxSize:          getExpandedSize(expectedSize),
				PreAllocatedSegmentIDs: &datapb.IDRange{
					Begin: startID + 1,
					End:   endID,
				},
			}
			err = t.compactionHandler.enqueueCompaction(task)
			if err != nil {
				log.Warn("failed to execute compaction task",
					zap.Int64("collection", group.collectionID),
					zap.Int64("triggerID", signal.id),
					zap.Int64("planID", task.GetPlanID()),
					zap.Int64s("inputSegments", inputSegmentIDs),
					zap.Error(err))
				continue
			}

			log.Info("time cost of generating global compaction",
				zap.Int64("time cost", time.Since(start).Milliseconds()),
				zap.Int64s("segmentIDs", inputSegmentIDs))
		}
	}
	return nil
}

// handleSignal processes segment flush caused partition-chan level compaction signal
func (t *compactionTrigger) handleSignal(signal *compactionSignal) {
	t.forceMu.Lock()
	defer t.forceMu.Unlock()

	// 1. check whether segment's binlogs should be compacted or not
	if t.compactionHandler.isFull() {
		log.Warn("compaction plan skipped due to handler full")
		return
	}

	segment := t.meta.GetHealthySegment(context.TODO(), signal.segmentID)
	if segment == nil {
		log.Warn("segment in compaction signal not found in meta", zap.Int64("segmentID", signal.segmentID))
		return
	}

	channel := segment.GetInsertChannel()
	partitionID := segment.GetPartitionID()
	collectionID := segment.GetCollectionID()
	segments := t.getCandidateSegments(channel, partitionID)

	if len(segments) == 0 {
		log.Info("the number of candidate segments is 0, skip to handle compaction")
		return
	}

	coll, err := t.getCollection(collectionID)
	if err != nil {
		log.Warn("get collection info failed, skip handling compaction",
			zap.Int64("collectionID", collectionID),
			zap.Int64("partitionID", partitionID),
			zap.String("channel", channel),
			zap.Error(err),
		)
		return
	}

	if !signal.isForce && !isCollectionAutoCompactionEnabled(coll) {
		log.RatedInfo(20, "collection auto compaction disabled",
			zap.Int64("collectionID", collectionID),
		)
		return
	}
	ts := tsoutil.ComposeTSByTime(time.Now(), 0)
	ct, err := getCompactTime(ts, coll)
	if err != nil {
		log.Warn("get compact time failed, skip to handle compaction", zap.Int64("collectionID", segment.GetCollectionID()),
			zap.Int64("partitionID", partitionID), zap.String("channel", channel))
		return
	}

	expectedSize := getExpectedSegmentSize(t.meta, coll)
	plans := t.generatePlans(segments, signal, ct, expectedSize)
	for _, plan := range plans {
		if t.compactionHandler.isFull() {
			log.Warn("compaction plan skipped due to handler full", zap.Int64("collection", signal.collectionID))
			break
		}

		// TODO[GOOSE], 11 = 1 planID + 10 segmentID, this is a hack need to be removed.
		// Any plan that output segment number greater than 10 will be marked as invalid plan for now.
		startID, endID, err := t.allocator.AllocN(11)
		if err != nil {
			log.Warn("fail to allocate id", zap.Error(err))
			return
		}
		totalRows, inputSegmentIDs := plan.A, plan.B
		start := time.Now()
		pts, _ := tsoutil.ParseTS(ct.startTime)
		task := &datapb.CompactionTask{
			PlanID:           startID,
			TriggerID:        signal.id,
			State:            datapb.CompactionTaskState_pipelining,
			StartTime:        pts.Unix(),
			TimeoutInSeconds: Params.DataCoordCfg.CompactionTimeoutInSeconds.GetAsInt32(),
			Type:             datapb.CompactionType_MixCompaction,
			CollectionTtl:    ct.collectionTTL.Nanoseconds(),
			CollectionID:     collectionID,
			PartitionID:      partitionID,
			Channel:          channel,
			InputSegments:    inputSegmentIDs,
			ResultSegments:   []int64{},
			TotalRows:        totalRows,
			Schema:           coll.Schema,
			MaxSize:          getExpandedSize(expectedSize),
			PreAllocatedSegmentIDs: &datapb.IDRange{
				Begin: startID + 1,
				End:   endID,
			},
		}
		if err := t.compactionHandler.enqueueCompaction(task); err != nil {
			log.Warn("failed to execute compaction task",
				zap.Int64("collection", collectionID),
				zap.Int64("triggerID", signal.id),
				zap.Int64("planID", task.GetPlanID()),
				zap.Int64s("inputSegments", inputSegmentIDs),
				zap.Error(err))
			continue
		}
		log.Info("time cost of generating compaction",
			zap.Int64("planID", task.GetPlanID()),
			zap.Int64("time cost", time.Since(start).Milliseconds()),
			zap.Int64("collectionID", signal.collectionID),
			zap.String("channel", channel),
			zap.Int64("partitionID", partitionID),
			zap.Int64s("inputSegmentIDs", inputSegmentIDs))
	}
}

func (t *compactionTrigger) generatePlans(segments []*SegmentInfo, signal *compactionSignal, compactTime *compactTime, expectedSize int64) []*typeutil.Pair[int64, []int64] {
	if len(segments) == 0 {
		log.Warn("the number of candidate segments is 0, skip to generate compaction plan")
		return []*typeutil.Pair[int64, []int64]{}
	}

	// find segments need internal compaction
	// TODO add low priority candidates, for example if the segment is smaller than full 0.9 * max segment size but larger than small segment boundary, we only execute compaction when there are no compaction running actively
	var prioritizedCandidates []*SegmentInfo
	var smallCandidates []*SegmentInfo
	var nonPlannedSegments []*SegmentInfo

	// TODO, currently we lack of the measurement of data distribution, there should be another compaction help on redistributing segment based on scalar/vector field distribution
	for _, segment := range segments {
		segment := segment.ShadowClone()
		// TODO should we trigger compaction periodically even if the segment has no obvious reason to be compacted?
		if signal.isForce || t.ShouldDoSingleCompaction(segment, compactTime) {
			prioritizedCandidates = append(prioritizedCandidates, segment)
		} else if t.isSmallSegment(segment, expectedSize) {
			smallCandidates = append(smallCandidates, segment)
		} else {
			nonPlannedSegments = append(nonPlannedSegments, segment)
		}
	}

	buckets := [][]*SegmentInfo{}
	toUpdate := newSegmentPacker("update", prioritizedCandidates)
	toMerge := newSegmentPacker("merge", smallCandidates)
	toPack := newSegmentPacker("pack", nonPlannedSegments)

	maxSegs := int64(4096) // Deprecate the max segment limit since it is irrelevant in simple compactions.
	minSegs := Params.DataCoordCfg.MinSegmentToMerge.GetAsInt64()
	compactableProportion := Params.DataCoordCfg.SegmentCompactableProportion.GetAsFloat()
	satisfiedSize := int64(float64(expectedSize) * compactableProportion)
	expantionRate := Params.DataCoordCfg.SegmentExpansionRate.GetAsFloat()
	maxLeftSize := expectedSize - satisfiedSize
	expectedExpandedSize := int64(float64(expectedSize) * expantionRate)
	maxExpandedLeftSize := expectedExpandedSize - satisfiedSize
	reasons := make([]string, 0)
	// 1. Merge small segments if they can make a full bucket
	for {
		pack, left := toMerge.pack(expectedSize, maxLeftSize, minSegs, maxSegs)
		if len(pack) == 0 {
			break
		}
		reasons = append(reasons, fmt.Sprintf("merging %d small segments with left size %d", len(pack), left))
		buckets = append(buckets, pack)
	}

	// 2. Pack prioritized candidates with small segments
	// TODO the compaction selection policy should consider if compaction workload is high
	for {
		// No limit on the remaining size because we want to pack all prioritized candidates
		pack, _ := toUpdate.packWith(expectedSize, math.MaxInt64, 0, maxSegs, toMerge)
		if len(pack) == 0 {
			break
		}
		reasons = append(reasons, fmt.Sprintf("packing %d prioritized segments", len(pack)))
		buckets = append(buckets, pack)
	}
	// if there is any segment toUpdate left, its size must greater than expectedSize, add it to the buckets
	for _, s := range toUpdate.candidates {
		buckets = append(buckets, []*SegmentInfo{s})
		reasons = append(reasons, fmt.Sprintf("force packing prioritized segment %d", s.GetID()))
	}
	// 2.+ legacy: squeeze small segments
	// Try merge all small segments, and then squeeze
	for {
		pack, _ := toMerge.pack(expectedSize, math.MaxInt64, minSegs, maxSegs)
		if len(pack) == 0 {
			break
		}
		reasons = append(reasons, fmt.Sprintf("packing all %d small segments", len(pack)))
		buckets = append(buckets, pack)
	}
	remaining := t.squeezeSmallSegmentsToBuckets(toMerge.candidates, buckets, expectedSize)
	toMerge = newSegmentPacker("merge", remaining)

	// 3. pack remaining small segments with non-planned segments
	for {
		pack, _ := toMerge.packWith(expectedExpandedSize, maxExpandedLeftSize, minSegs, maxSegs, toPack)
		if len(pack) == 0 {
			break
		}
		reasons = append(reasons, fmt.Sprintf("packing %d small segments and non-planned segments", len(pack)))
		buckets = append(buckets, pack)
	}

	tasks := make([]*typeutil.Pair[int64, []int64], len(buckets))
	for i, b := range buckets {
		segmentIDs := make([]int64, 0)
		var totalRows int64
		for _, s := range b {
			totalRows += s.GetNumOfRows()
			segmentIDs = append(segmentIDs, s.GetID())
		}
		pair := typeutil.NewPair(totalRows, segmentIDs)
		tasks[i] = &pair
	}

	if len(tasks) > 0 {
		log.Info("generated nontrivial compaction tasks",
			zap.Int64("collectionID", signal.collectionID),
			zap.Int("prioritizedCandidates", len(prioritizedCandidates)),
			zap.Int("smallCandidates", len(smallCandidates)),
			zap.Int("nonPlannedSegments", len(nonPlannedSegments)),
			zap.Strings("reasons", reasons))
	}
	return tasks
}

func (t *compactionTrigger) getCandidateSegments(channel string, partitionID UniqueID) []*SegmentInfo {
	segments := t.meta.GetSegmentsByChannel(channel)
	if Params.DataCoordCfg.IndexBasedCompaction.GetAsBool() {
		segments = FilterInIndexedSegments(t.handler, t.meta, false, segments...)
	}

	var res []*SegmentInfo
	for _, s := range segments {
		if !isSegmentHealthy(s) ||
			!isFlush(s) ||
			s.GetInsertChannel() != channel ||
			s.GetPartitionID() != partitionID ||
			s.isCompacting ||
			s.GetIsImporting() ||
			s.GetLevel() == datapb.SegmentLevel_L0 ||
			s.GetLevel() == datapb.SegmentLevel_L2 {
			continue
		}
		res = append(res, s)
	}

	return res
}

func (t *compactionTrigger) isSmallSegment(segment *SegmentInfo, expectedSize int64) bool {
	return segment.getSegmentSize() < int64(float64(expectedSize)*Params.DataCoordCfg.SegmentSmallProportion.GetAsFloat())
}

func (t *compactionTrigger) isCompactableSegment(targetSize, expectedSize int64) bool {
	smallProportion := Params.DataCoordCfg.SegmentSmallProportion.GetAsFloat()
	compactableProportion := Params.DataCoordCfg.SegmentCompactableProportion.GetAsFloat()

	// avoid invalid single segment compaction
	if compactableProportion < smallProportion {
		compactableProportion = smallProportion
	}

	return targetSize > int64(float64(expectedSize)*compactableProportion)
}

func isExpandableSmallSegment(segment *SegmentInfo, expectedSize int64) bool {
	return segment.getSegmentSize() < int64(float64(expectedSize)*(Params.DataCoordCfg.SegmentExpansionRate.GetAsFloat()-1))
}

func isDeltalogTooManySegment(segment *SegmentInfo) bool {
	deltaLogCount := GetBinlogCount(segment.GetDeltalogs())
	return deltaLogCount > Params.DataCoordCfg.SingleCompactionDeltalogMaxNum.GetAsInt()
}

func isDeleteRowsTooManySegment(segment *SegmentInfo) bool {
	totalDeletedRows := 0
	totalDeleteLogSize := int64(0)
	for _, deltaLogs := range segment.GetDeltalogs() {
		for _, l := range deltaLogs.GetBinlogs() {
			totalDeletedRows += int(l.GetEntriesNum())
			totalDeleteLogSize += l.GetMemorySize()
		}
	}

	// currently delta log size and delete ratio policy is applied
	is := float64(totalDeletedRows)/float64(segment.GetNumOfRows()) >= Params.DataCoordCfg.SingleCompactionRatioThreshold.GetAsFloat() ||
		totalDeleteLogSize > Params.DataCoordCfg.SingleCompactionDeltaLogMaxSize.GetAsInt64()
	if is {
		log.Ctx(context.TODO()).Info("total delete entities is too much",
			zap.Int64("segmentID", segment.ID),
			zap.Int64("numRows", segment.GetNumOfRows()),
			zap.Int("deleted rows", totalDeletedRows),
			zap.Int64("delete log size", totalDeleteLogSize))
	}
	return is
}

func (t *compactionTrigger) ShouldDoSingleCompaction(segment *SegmentInfo, compactTime *compactTime) bool {
	// no longer restricted binlog numbers because this is now related to field numbers

	log := log.Ctx(context.TODO())
	binlogCount := GetBinlogCount(segment.GetBinlogs())
	deltaLogCount := GetBinlogCount(segment.GetDeltalogs())
	if isDeltalogTooManySegment(segment) {
		log.Info("total delta number is too much, trigger compaction", zap.Int64("segmentID", segment.ID), zap.Int("Bin logs", binlogCount), zap.Int("Delta logs", deltaLogCount))
		return true
	}

	// if expire time is enabled, put segment into compaction candidate
	totalExpiredSize := int64(0)
	totalExpiredRows := 0
	for _, binlogs := range segment.GetBinlogs() {
		for _, l := range binlogs.GetBinlogs() {
			// TODO, we should probably estimate expired log entries by total rows in binlog and the ralationship of timeTo, timeFrom and expire time
			if l.TimestampTo < compactTime.expireTime {
				log.RatedDebug(10, "mark binlog as expired",
					zap.Int64("segmentID", segment.ID),
					zap.Int64("binlogID", l.GetLogID()),
					zap.Uint64("binlogTimestampTo", l.TimestampTo),
					zap.Uint64("compactExpireTime", compactTime.expireTime))
				totalExpiredRows += int(l.GetEntriesNum())
				totalExpiredSize += l.GetMemorySize()
			}
		}
	}

	if float64(totalExpiredRows)/float64(segment.GetNumOfRows()) >= Params.DataCoordCfg.SingleCompactionRatioThreshold.GetAsFloat() ||
		totalExpiredSize > Params.DataCoordCfg.SingleCompactionExpiredLogMaxSize.GetAsInt64() {
		log.Info("total expired entities is too much, trigger compaction", zap.Int64("segmentID", segment.ID),
			zap.Int("expiredRows", totalExpiredRows), zap.Int64("expiredLogSize", totalExpiredSize),
			zap.Bool("createdByCompaction", segment.CreatedByCompaction), zap.Int64s("compactionFrom", segment.CompactionFrom))
		return true
	}

	// currently delta log size and delete ratio policy is applied
	if isDeleteRowsTooManySegment(segment) {
		return true
	}

	if Params.DataCoordCfg.AutoUpgradeSegmentIndex.GetAsBool() {
		// index version of segment lower than current version and IndexFileKeys should have value, trigger compaction
		indexIDToSegIdxes := t.meta.indexMeta.GetSegmentIndexes(segment.CollectionID, segment.ID)
		for _, index := range indexIDToSegIdxes {
			if index.CurrentIndexVersion < t.indexEngineVersionManager.GetCurrentIndexEngineVersion() &&
				len(index.IndexFileKeys) > 0 {
				log.Info("index version is too old, trigger compaction",
					zap.Int64("segmentID", segment.ID),
					zap.Int64("indexID", index.IndexID),
					zap.Strings("indexFileKeys", index.IndexFileKeys),
					zap.Int32("currentIndexVersion", index.CurrentIndexVersion),
					zap.Int32("currentEngineVersion", t.indexEngineVersionManager.GetCurrentIndexEngineVersion()))
				return true
			}
		}
	}

	return false
}

func isFlush(segment *SegmentInfo) bool {
	return segment.GetState() == commonpb.SegmentState_Flushed || segment.GetState() == commonpb.SegmentState_Flushing
}

func needSync(segment *SegmentInfo) bool {
	return segment.GetState() == commonpb.SegmentState_Flushed || segment.GetState() == commonpb.SegmentState_Flushing || segment.GetState() == commonpb.SegmentState_Sealed
}

// buckets will be updated inplace
func (t *compactionTrigger) squeezeSmallSegmentsToBuckets(small []*SegmentInfo, buckets [][]*SegmentInfo, expectedSize int64) (remaining []*SegmentInfo) {
	for i := len(small) - 1; i >= 0; i-- {
		s := small[i]
		if !isExpandableSmallSegment(s, expectedSize) {
			continue
		}
		// Try squeeze this segment into existing plans. This could cause segment size to exceed maxSize.
		for bidx, b := range buckets {
			totalSize := lo.SumBy(b, func(s *SegmentInfo) int64 { return s.getSegmentSize() })
			if totalSize+s.getSegmentSize() > int64(Params.DataCoordCfg.SegmentExpansionRate.GetAsFloat()*float64(expectedSize)) {
				continue
			}
			buckets[bidx] = append(buckets[bidx], s)

			small = append(small[:i], small[i+1:]...)
			break
		}
	}

	return small
}

func getExpandedSize(size int64) int64 {
	return int64(float64(size) * Params.DataCoordCfg.SegmentExpansionRate.GetAsFloat())
}
