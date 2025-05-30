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
	"testing"

	"github.com/cockroachdb/errors"
	"github.com/samber/lo"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/suite"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"github.com/milvus-io/milvus-proto/go-api/v2/schemapb"
	globalIDAllocator "github.com/milvus-io/milvus/internal/allocator"
	kvmock "github.com/milvus-io/milvus/internal/kv/mocks"
	"github.com/milvus-io/milvus/pkg/v2/kv/predicates"
	"github.com/milvus-io/milvus/pkg/v2/log"
	"github.com/milvus-io/milvus/pkg/v2/proto/datapb"
	"github.com/milvus-io/milvus/pkg/v2/util/merr"
	"github.com/milvus-io/milvus/pkg/v2/util/paramtable"
)

func TestChannelManagerSuite(t *testing.T) {
	suite.Run(t, new(ChannelManagerSuite))
}

type ChannelManagerSuite struct {
	suite.Suite

	mockKv      *kvmock.MetaKv
	mockCluster *MockSubCluster
	mockAlloc   *globalIDAllocator.MockGlobalIDAllocator
	mockHandler *NMockHandler
}

func (s *ChannelManagerSuite) prepareMeta(chNodes map[string]int64, state datapb.ChannelWatchState) {
	s.SetupTest()
	if chNodes == nil {
		s.mockKv.EXPECT().LoadWithPrefix(mock.Anything, mock.Anything).Return(nil, nil, nil).Once()
		return
	}
	var keys, values []string
	for channel, nodeID := range chNodes {
		keys = append(keys, fmt.Sprintf("channel_store/%d/%s", nodeID, channel))
		info := generateWatchInfo(channel, state)
		bs, err := proto.Marshal(info)
		s.Require().NoError(err)
		values = append(values, string(bs))
	}
	s.mockKv.EXPECT().LoadWithPrefix(mock.Anything, mock.Anything).Return(keys, values, nil).Once()
}

func (s *ChannelManagerSuite) checkAssignment(m *ChannelManagerImpl, nodeID int64, channel string, state ChannelState) {
	rwChannel, found := m.GetChannel(nodeID, channel)
	s.True(found)
	s.NotNil(rwChannel)
	s.Equal(channel, rwChannel.GetName())
	sChannel, ok := rwChannel.(*StateChannel)
	s.True(ok)
	s.Equal(state, sChannel.currentState)
	s.EqualValues(nodeID, sChannel.assignedNode)
	s.True(m.Match(nodeID, channel))

	if nodeID != bufferID {
		gotNode, err := m.FindWatcher(channel)
		s.NoError(err)
		s.EqualValues(gotNode, nodeID)
	}
}

func (s *ChannelManagerSuite) checkNoAssignment(m *ChannelManagerImpl, nodeID int64, channel string) {
	rwChannel, found := m.GetChannel(nodeID, channel)
	s.False(found)
	s.Nil(rwChannel)
	s.False(m.Match(nodeID, channel))
}

func (s *ChannelManagerSuite) SetupTest() {
	s.mockKv = kvmock.NewMetaKv(s.T())
	s.mockCluster = NewMockSubCluster(s.T())
	s.mockHandler = NewNMockHandler(s.T())
	s.mockHandler.EXPECT().GetDataVChanPositions(mock.Anything, mock.Anything).
		RunAndReturn(func(ch RWChannel, partitionID UniqueID) *datapb.VchannelInfo {
			return &datapb.VchannelInfo{
				CollectionID: ch.GetCollectionID(),
				ChannelName:  ch.GetName(),
			}
		}).Maybe()
	s.mockKv.EXPECT().MultiSaveAndRemove(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(
		func(ctx context.Context, save map[string]string, removals []string, preds ...predicates.Predicate) error {
			log.Info("test save and remove", zap.Any("save", save), zap.Any("removals", removals))
			return nil
		}).Maybe()
	s.mockAlloc = globalIDAllocator.NewMockGlobalIDAllocator(s.T())
	s.mockAlloc.EXPECT().AllocOne().Return(1, nil).Maybe()
}

func (s *ChannelManagerSuite) TearDownTest() {}

func (s *ChannelManagerSuite) TestAddNode() {
	s.Run("AddNode with empty store", func() {
		s.prepareMeta(nil, 0)
		m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, s.mockAlloc)
		s.Require().NoError(err)

		var testNode int64 = 1
		err = m.AddNode(testNode)
		s.NoError(err)

		info := m.store.GetNode(testNode)
		s.NotNil(info)
		s.Empty(info.Channels)
		s.Equal(info.NodeID, testNode)
	})
	s.Run("AddNode with channel in bufferID", func() {
		chNodes := map[string]int64{
			"ch1": bufferID,
			"ch2": bufferID,
		}
		s.prepareMeta(chNodes, datapb.ChannelWatchState_ToWatch)
		m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, s.mockAlloc)
		s.Require().NoError(err)

		var (
			testNodeID   int64 = 1
			testChannels       = []string{"ch1", "ch2"}
		)
		lo.ForEach(testChannels, func(ch string, _ int) {
			s.checkAssignment(m, bufferID, ch, Standby)
		})

		err = m.AddNode(testNodeID)
		s.NoError(err)

		info := m.store.GetNode(testNodeID)
		s.NotNil(info)
		s.Empty(info.Channels)
		s.Equal(info.NodeID, testNodeID)
	})
	s.Run("AddNode with channels evenly in other node", func() {
		var (
			testNodeID   int64 = 100
			storedNodeID int64 = 1
			testChannel        = "ch1"
		)

		chNodes := map[string]int64{testChannel: storedNodeID}
		s.prepareMeta(chNodes, datapb.ChannelWatchState_WatchSuccess)

		m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, s.mockAlloc)
		s.Require().NoError(err)

		s.checkAssignment(m, storedNodeID, testChannel, Watched)

		err = m.AddNode(testNodeID)
		s.NoError(err)
		s.ElementsMatch([]int64{100, 1}, m.store.GetNodes())
		s.checkNoAssignment(m, testNodeID, testChannel)

		testNodeID = 101
		paramtable.Get().Save(paramtable.Get().DataCoordCfg.AutoBalance.Key, "true")
		defer paramtable.Get().Reset(paramtable.Get().DataCoordCfg.AutoBalance.Key)

		err = m.AddNode(testNodeID)
		s.NoError(err)
		s.ElementsMatch([]int64{100, 101, 1}, m.store.GetNodes())
		s.checkNoAssignment(m, testNodeID, testChannel)
	})
	s.Run("AddNode with channels unevenly in other node", func() {
		chNodes := map[string]int64{
			"ch1": 1,
			"ch2": 1,
			"ch3": 1,
		}
		s.prepareMeta(chNodes, datapb.ChannelWatchState_WatchSuccess)

		m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, s.mockAlloc)
		s.Require().NoError(err)

		var testNodeID int64 = 100
		paramtable.Get().Save(paramtable.Get().DataCoordCfg.AutoBalance.Key, "true")
		defer paramtable.Get().Reset(paramtable.Get().DataCoordCfg.AutoBalance.Key)

		err = m.AddNode(testNodeID)
		s.NoError(err)
		s.ElementsMatch([]int64{testNodeID, 1}, m.store.GetNodes())
	})
}

func (s *ChannelManagerSuite) TestWatch() {
	s.Run("test Watch with empty store", func() {
		s.prepareMeta(nil, 0)
		m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, s.mockAlloc)
		s.Require().NoError(err)

		var testCh string = "ch1"

		err = m.Watch(context.TODO(), getChannel(testCh, 1))
		s.NoError(err)

		s.checkAssignment(m, bufferID, testCh, Standby)
	})
	s.Run("test Watch with nodeID in store", func() {
		s.prepareMeta(nil, 0)
		m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, s.mockAlloc)
		s.Require().NoError(err)

		var (
			testCh     string = "ch1"
			testNodeID int64  = 1
		)
		err = m.AddNode(testNodeID)
		s.NoError(err)
		s.checkNoAssignment(m, testNodeID, testCh)

		err = m.Watch(context.TODO(), getChannel(testCh, 1))
		s.NoError(err)

		s.checkAssignment(m, testNodeID, testCh, ToWatch)
	})
}

func (s *ChannelManagerSuite) TestRelease() {
	s.Run("release not exist nodeID and channel", func() {
		s.prepareMeta(nil, 0)
		m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, s.mockAlloc)
		s.Require().NoError(err)

		err = m.Release(1, "ch1")
		s.Error(err)
		log.Info("error", zap.String("msg", err.Error()))

		m.AddNode(1)
		err = m.Release(1, "ch1")
		s.Error(err)
		log.Info("error", zap.String("msg", err.Error()))
	})

	s.Run("release channel in bufferID", func() {
		s.prepareMeta(nil, 0)
		m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, s.mockAlloc)
		s.Require().NoError(err)

		m.Watch(context.TODO(), getChannel("ch1", 1))
		s.checkAssignment(m, bufferID, "ch1", Standby)

		err = m.Release(bufferID, "ch1")
		s.NoError(err)
		s.checkAssignment(m, bufferID, "ch1", Standby)
	})
}

func (s *ChannelManagerSuite) TestDeleteNode() {
	s.Run("delete not exsit node", func() {
		s.prepareMeta(nil, 0)
		m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, s.mockAlloc)
		s.Require().NoError(err)
		info := m.store.GetNode(1)
		s.Require().Nil(info)

		err = m.DeleteNode(1)
		s.NoError(err)
	})
	s.Run("delete bufferID", func() {
		s.prepareMeta(nil, 0)
		m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, s.mockAlloc)
		s.Require().NoError(err)
		info := m.store.GetNode(bufferID)
		s.Require().NotNil(info)

		err = m.DeleteNode(bufferID)
		s.NoError(err)
	})

	s.Run("delete node without assigment", func() {
		s.prepareMeta(nil, 0)
		m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, s.mockAlloc)
		s.Require().NoError(err)

		err = m.AddNode(1)
		s.NoError(err)
		info := m.store.GetNode(bufferID)
		s.Require().NotNil(info)

		err = m.DeleteNode(1)
		s.NoError(err)
		info = m.store.GetNode(1)
		s.Nil(info)
	})
	s.Run("delete node with channel", func() {
		chNodes := map[string]int64{
			"ch1": 1,
			"ch2": 1,
			"ch3": 1,
		}
		s.prepareMeta(chNodes, datapb.ChannelWatchState_WatchSuccess)
		m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, s.mockAlloc)
		s.Require().NoError(err)
		s.checkAssignment(m, 1, "ch1", Watched)
		s.checkAssignment(m, 1, "ch2", Watched)
		s.checkAssignment(m, 1, "ch3", Watched)

		err = m.AddNode(2)
		s.NoError(err)

		err = m.DeleteNode(1)
		s.NoError(err)
		info := m.store.GetNode(bufferID)
		s.NotNil(info)

		s.Equal(3, len(info.Channels))
		s.EqualValues(bufferID, info.NodeID)
		s.checkAssignment(m, bufferID, "ch1", Standby)
		s.checkAssignment(m, bufferID, "ch2", Standby)
		s.checkAssignment(m, bufferID, "ch3", Standby)

		info = m.store.GetNode(1)
		s.Nil(info)
	})
}

func (s *ChannelManagerSuite) TestFindWatcher() {
	chNodes := map[string]int64{
		"ch1": bufferID,
		"ch2": bufferID,
		"ch3": 1,
		"ch4": 1,
	}
	s.prepareMeta(chNodes, datapb.ChannelWatchState_WatchSuccess)
	m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, s.mockAlloc)
	s.Require().NoError(err)

	tests := []struct {
		description string
		testCh      string

		outNodeID int64
		outError  bool
	}{
		{"channel not exist", "ch-notexist", 0, true},
		{"channel in bufferID", "ch1", bufferID, true},
		{"channel in bufferID", "ch2", bufferID, true},
		{"channel in nodeID=1", "ch3", 1, false},
		{"channel in nodeID=1", "ch4", 1, false},
	}

	for _, test := range tests {
		s.Run(test.description, func() {
			gotID, gotErr := m.FindWatcher(test.testCh)
			s.EqualValues(test.outNodeID, gotID)
			if test.outError {
				s.Error(gotErr)
			} else {
				s.NoError(gotErr)
			}
		})
	}
}

func (s *ChannelManagerSuite) TestAdvanceChannelState() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Run("advance towatch dn watched to torelease", func() {
		chNodes := map[string]int64{
			"ch1": 1,
			"ch2": 1,
		}
		s.prepareMeta(chNodes, datapb.ChannelWatchState_ToWatch)
		s.mockCluster.EXPECT().NotifyChannelOperation(mock.Anything, mock.Anything, mock.Anything).
			RunAndReturn(func(ctx context.Context, nodeID int64, req *datapb.ChannelOperationsRequest) error {
				s.Require().Equal(1, len(req.GetInfos()))
				switch req.GetInfos()[0].GetVchan().GetChannelName() {
				case "ch2":
					return merr.WrapErrChannelReduplicate("ch2")
				default:
					return nil
				}
			}).Twice()
		m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, s.mockAlloc)
		s.Require().NoError(err)
		s.checkAssignment(m, 1, "ch1", ToWatch)
		s.checkAssignment(m, 1, "ch2", ToWatch)

		m.AdvanceChannelState(ctx)
		s.checkAssignment(m, 1, "ch1", Watching)
		s.checkAssignment(m, 1, "ch2", ToRelease)
	})
	s.Run("advance statndby with no available nodes", func() {
		chNodes := map[string]int64{
			"ch1": bufferID,
			"ch2": bufferID,
		}
		s.prepareMeta(chNodes, datapb.ChannelWatchState_ToWatch)
		s.mockHandler.EXPECT().CheckShouldDropChannel(mock.Anything).Return(false)
		m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, s.mockAlloc)
		s.Require().NoError(err)
		s.checkAssignment(m, bufferID, "ch1", Standby)
		s.checkAssignment(m, bufferID, "ch2", Standby)

		m.AdvanceChannelState(ctx)
		s.checkAssignment(m, bufferID, "ch1", Standby)
		s.checkAssignment(m, bufferID, "ch2", Standby)
	})

	s.Run("advance statndby with node 1", func() {
		chNodes := map[string]int64{
			"ch1": bufferID,
			"ch2": bufferID,
			"ch3": 1,
		}
		s.prepareMeta(chNodes, datapb.ChannelWatchState_WatchSuccess)
		s.mockHandler.EXPECT().CheckShouldDropChannel(mock.Anything).Return(false).Times(2)
		m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, s.mockAlloc)
		s.Require().NoError(err)
		s.checkAssignment(m, bufferID, "ch1", Standby)
		s.checkAssignment(m, bufferID, "ch2", Standby)
		s.checkAssignment(m, 1, "ch3", Watched)

		m.AdvanceChannelState(ctx)
		s.checkAssignment(m, 1, "ch1", ToWatch)
		s.checkAssignment(m, 1, "ch2", ToWatch)
	})
	s.Run("advance towatch channels notify success check success", func() {
		chNodes := map[string]int64{
			"ch1": 1,
			"ch2": 1,
		}
		s.prepareMeta(chNodes, datapb.ChannelWatchState_ToWatch)
		s.mockCluster.EXPECT().NotifyChannelOperation(mock.Anything, mock.Anything, mock.Anything).Return(nil).Twice()
		m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, s.mockAlloc)
		s.Require().NoError(err)
		s.checkAssignment(m, 1, "ch1", ToWatch)
		s.checkAssignment(m, 1, "ch2", ToWatch)

		m.AdvanceChannelState(ctx)
		s.checkAssignment(m, 1, "ch1", Watching)
		s.checkAssignment(m, 1, "ch2", Watching)
	})
	s.Run("advance watching channels check no progress", func() {
		chNodes := map[string]int64{
			"ch1": 1,
			"ch2": 1,
		}
		s.prepareMeta(chNodes, datapb.ChannelWatchState_ToWatch)
		s.mockCluster.EXPECT().NotifyChannelOperation(mock.Anything, mock.Anything, mock.Anything).Return(nil).Twice()
		m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, s.mockAlloc)
		s.Require().NoError(err)
		s.checkAssignment(m, 1, "ch1", ToWatch)
		s.checkAssignment(m, 1, "ch2", ToWatch)

		m.AdvanceChannelState(ctx)
		s.checkAssignment(m, 1, "ch1", Watching)
		s.checkAssignment(m, 1, "ch2", Watching)

		s.mockCluster.EXPECT().CheckChannelOperationProgress(mock.Anything, mock.Anything, mock.Anything).
			Return(&datapb.ChannelOperationProgressResponse{State: datapb.ChannelWatchState_ToWatch}, nil).Twice()
		m.AdvanceChannelState(ctx)
		s.checkAssignment(m, 1, "ch1", Watching)
		s.checkAssignment(m, 1, "ch2", Watching)
	})

	s.Run("advance watching channels released during check", func() {
		idx := int64(19530)
		mockAlloc := globalIDAllocator.NewMockGlobalIDAllocator(s.T())
		mockAlloc.EXPECT().AllocOne().
			RunAndReturn(func() (int64, error) {
				idx++
				return idx, nil
			})
		chNodes := map[string]int64{
			"ch1": 1,
			"ch2": 1,
		}
		s.prepareMeta(chNodes, datapb.ChannelWatchState_ToWatch)
		s.mockCluster.EXPECT().NotifyChannelOperation(mock.Anything, mock.Anything, mock.Anything).Return(nil).Twice()
		m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, mockAlloc)
		s.Require().NoError(err)
		s.checkAssignment(m, 1, "ch1", ToWatch)
		s.checkAssignment(m, 1, "ch2", ToWatch)

		m.AdvanceChannelState(ctx)
		s.checkAssignment(m, 1, "ch1", Watching)
		s.checkAssignment(m, 1, "ch2", Watching)

		// Release belfore check return
		s.mockCluster.EXPECT().CheckChannelOperationProgress(mock.Anything, mock.Anything, mock.Anything).RunAndReturn(func(ctx context.Context, nodeID int64, info *datapb.ChannelWatchInfo) (*datapb.ChannelOperationProgressResponse, error) {
			if info.GetVchan().GetChannelName() == "ch1" {
				m.Release(1, "ch1")
				s.checkAssignment(m, 1, "ch1", ToRelease)
				rwChannel, found := m.GetChannel(nodeID, "ch1")
				s.True(found)
				metaInfo := rwChannel.GetWatchInfo()
				s.Require().EqualValues(metaInfo.GetOpID(), 19531)
				log.Info("Trying to check this info", zap.Any("meta info", rwChannel.GetWatchInfo()))
			}
			log.Info("Trying to check this info", zap.Any("rpc info", info))
			return &datapb.ChannelOperationProgressResponse{State: datapb.ChannelWatchState_WatchSuccess, Progress: 100}, nil
		}).Twice()
		m.AdvanceChannelState(ctx)

		s.checkAssignment(m, 1, "ch1", ToRelease)
		s.checkAssignment(m, 1, "ch2", Watched)
	})

	s.Run("advance watching channels check ErrNodeNotFound", func() {
		chNodes := map[string]int64{
			"ch1": 1,
			"ch2": 1,
		}
		s.prepareMeta(chNodes, datapb.ChannelWatchState_ToWatch)
		s.mockCluster.EXPECT().NotifyChannelOperation(mock.Anything, mock.Anything, mock.Anything).Return(nil).Twice()
		m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, s.mockAlloc)
		s.Require().NoError(err)
		s.checkAssignment(m, 1, "ch1", ToWatch)
		s.checkAssignment(m, 1, "ch2", ToWatch)

		m.AdvanceChannelState(ctx)
		s.checkAssignment(m, 1, "ch1", Watching)
		s.checkAssignment(m, 1, "ch2", Watching)

		s.mockCluster.EXPECT().CheckChannelOperationProgress(mock.Anything, mock.Anything, mock.Anything).
			Return(nil, merr.WrapErrNodeNotFound(1)).Twice()
		m.AdvanceChannelState(ctx)
		s.checkAssignment(m, 1, "ch1", Standby)
		s.checkAssignment(m, 1, "ch2", Standby)
	})

	s.Run("advance watching channels check watch success", func() {
		chNodes := map[string]int64{
			"ch1": 1,
			"ch2": 1,
		}
		s.prepareMeta(chNodes, datapb.ChannelWatchState_ToWatch)
		s.mockCluster.EXPECT().NotifyChannelOperation(mock.Anything, mock.Anything, mock.Anything).Return(nil).Twice()
		m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, s.mockAlloc)
		s.Require().NoError(err)
		s.checkAssignment(m, 1, "ch1", ToWatch)
		s.checkAssignment(m, 1, "ch2", ToWatch)

		m.AdvanceChannelState(ctx)
		s.checkAssignment(m, 1, "ch1", Watching)
		s.checkAssignment(m, 1, "ch2", Watching)

		s.mockCluster.EXPECT().CheckChannelOperationProgress(mock.Anything, mock.Anything, mock.Anything).
			Return(&datapb.ChannelOperationProgressResponse{State: datapb.ChannelWatchState_WatchSuccess}, nil).Twice()
		m.AdvanceChannelState(ctx)
		s.checkAssignment(m, 1, "ch1", Watched)
		s.checkAssignment(m, 1, "ch2", Watched)
	})
	s.Run("advance watching channels check watch fail", func() {
		chNodes := map[string]int64{
			"ch1": 1,
			"ch2": 1,
		}
		s.prepareMeta(chNodes, datapb.ChannelWatchState_ToWatch)
		s.mockCluster.EXPECT().NotifyChannelOperation(mock.Anything, mock.Anything, mock.Anything).Return(nil).Times(2)
		m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, s.mockAlloc)
		s.Require().NoError(err)
		s.checkAssignment(m, 1, "ch1", ToWatch)
		s.checkAssignment(m, 1, "ch2", ToWatch)

		m.AdvanceChannelState(ctx)
		s.checkAssignment(m, 1, "ch1", Watching)
		s.checkAssignment(m, 1, "ch2", Watching)

		s.mockCluster.EXPECT().CheckChannelOperationProgress(mock.Anything, mock.Anything, mock.Anything).
			Return(&datapb.ChannelOperationProgressResponse{State: datapb.ChannelWatchState_WatchFailure}, nil).Twice()
		m.AdvanceChannelState(ctx)
		s.checkAssignment(m, 1, "ch1", Standby)
		s.checkAssignment(m, 1, "ch2", Standby)

		s.mockHandler.EXPECT().CheckShouldDropChannel(mock.Anything).Return(false)
		m.AdvanceChannelState(ctx)
		s.checkAssignment(m, 1, "ch1", ToWatch)
		s.checkAssignment(m, 1, "ch2", ToWatch)
	})
	s.Run("advance releasing channels check release no progress", func() {
		chNodes := map[string]int64{
			"ch1": 1,
			"ch2": 1,
		}
		s.prepareMeta(chNodes, datapb.ChannelWatchState_ToRelease)
		s.mockCluster.EXPECT().NotifyChannelOperation(mock.Anything, mock.Anything, mock.Anything).Return(nil).Twice()
		m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, s.mockAlloc)
		s.Require().NoError(err)
		s.checkAssignment(m, 1, "ch1", ToRelease)
		s.checkAssignment(m, 1, "ch2", ToRelease)

		m.AdvanceChannelState(ctx)
		s.checkAssignment(m, 1, "ch1", Releasing)
		s.checkAssignment(m, 1, "ch2", Releasing)

		s.mockCluster.EXPECT().CheckChannelOperationProgress(mock.Anything, mock.Anything, mock.Anything).
			Return(&datapb.ChannelOperationProgressResponse{State: datapb.ChannelWatchState_ToRelease}, nil).Twice()
		m.AdvanceChannelState(ctx)
		s.checkAssignment(m, 1, "ch1", Releasing)
		s.checkAssignment(m, 1, "ch2", Releasing)
	})
	s.Run("advance releasing channels check ErrNodeNotFound", func() {
		chNodes := map[string]int64{
			"ch1": 1,
			"ch2": 1,
		}
		s.prepareMeta(chNodes, datapb.ChannelWatchState_ToRelease)
		s.mockCluster.EXPECT().NotifyChannelOperation(mock.Anything, mock.Anything, mock.Anything).Return(nil).Twice()
		m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, s.mockAlloc)
		s.Require().NoError(err)
		s.checkAssignment(m, 1, "ch1", ToRelease)
		s.checkAssignment(m, 1, "ch2", ToRelease)

		m.AdvanceChannelState(ctx)
		s.checkAssignment(m, 1, "ch1", Releasing)
		s.checkAssignment(m, 1, "ch2", Releasing)

		s.mockCluster.EXPECT().CheckChannelOperationProgress(mock.Anything, mock.Anything, mock.Anything).
			Return(nil, merr.WrapErrNodeNotFound(1)).Twice()
		m.AdvanceChannelState(ctx)
		s.checkAssignment(m, 1, "ch1", Standby)
		s.checkAssignment(m, 1, "ch2", Standby)
	})
	s.Run("advance releasing channels check release success", func() {
		chNodes := map[string]int64{
			"ch1": 1,
			"ch2": 1,
		}
		s.prepareMeta(chNodes, datapb.ChannelWatchState_ToRelease)
		s.mockCluster.EXPECT().NotifyChannelOperation(mock.Anything, mock.Anything, mock.Anything).Return(nil).Twice()
		m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, s.mockAlloc)
		s.Require().NoError(err)
		s.checkAssignment(m, 1, "ch1", ToRelease)
		s.checkAssignment(m, 1, "ch2", ToRelease)

		m.AdvanceChannelState(ctx)
		s.checkAssignment(m, 1, "ch1", Releasing)
		s.checkAssignment(m, 1, "ch2", Releasing)

		s.mockCluster.EXPECT().CheckChannelOperationProgress(mock.Anything, mock.Anything, mock.Anything).
			Return(&datapb.ChannelOperationProgressResponse{State: datapb.ChannelWatchState_ReleaseSuccess}, nil).Twice()
		m.AdvanceChannelState(ctx)
		s.checkAssignment(m, 1, "ch1", Standby)
		s.checkAssignment(m, 1, "ch2", Standby)

		s.mockHandler.EXPECT().CheckShouldDropChannel(mock.Anything).Return(false)
		m.AdvanceChannelState(ctx)
		s.checkAssignment(m, 1, "ch1", ToWatch)
		s.checkAssignment(m, 1, "ch2", ToWatch)
	})
	s.Run("advance releasing channels check release fail", func() {
		chNodes := map[string]int64{
			"ch1": 1,
			"ch2": 1,
		}
		s.prepareMeta(chNodes, datapb.ChannelWatchState_ToRelease)
		s.mockCluster.EXPECT().NotifyChannelOperation(mock.Anything, mock.Anything, mock.Anything).Return(nil).Twice()
		m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, s.mockAlloc)
		s.Require().NoError(err)
		s.checkAssignment(m, 1, "ch1", ToRelease)
		s.checkAssignment(m, 1, "ch2", ToRelease)

		m.AdvanceChannelState(ctx)
		s.checkAssignment(m, 1, "ch1", Releasing)
		s.checkAssignment(m, 1, "ch2", Releasing)

		s.mockCluster.EXPECT().CheckChannelOperationProgress(mock.Anything, mock.Anything, mock.Anything).
			Return(&datapb.ChannelOperationProgressResponse{State: datapb.ChannelWatchState_ReleaseFailure}, nil).Twice()
		m.AdvanceChannelState(ctx)
		s.checkAssignment(m, 1, "ch1", Standby)
		s.checkAssignment(m, 1, "ch2", Standby)

		s.mockHandler.EXPECT().CheckShouldDropChannel(mock.Anything).Return(false)
		m.AdvanceChannelState(ctx)
		// TODO, donot assign to abnormal nodes
		s.checkAssignment(m, 1, "ch1", ToWatch)
		s.checkAssignment(m, 1, "ch2", ToWatch)
	})
	s.Run("advance towatch channels notify fail", func() {
		chNodes := map[string]int64{
			"ch1": 1,
			"ch2": 1,
		}
		s.prepareMeta(chNodes, datapb.ChannelWatchState_ToWatch)
		s.mockCluster.EXPECT().NotifyChannelOperation(mock.Anything, mock.Anything, mock.Anything).
			Return(errors.New("mock error")).Twice()
		m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, s.mockAlloc)
		s.Require().NoError(err)
		s.checkAssignment(m, 1, "ch1", ToWatch)
		s.checkAssignment(m, 1, "ch2", ToWatch)

		m.AdvanceChannelState(ctx)
		s.checkAssignment(m, 1, "ch1", Standby)
		s.checkAssignment(m, 1, "ch2", Standby)
	})
	s.Run("advance to release channels notify success", func() {
		chNodes := map[string]int64{
			"ch1": 1,
			"ch2": 1,
		}
		s.prepareMeta(chNodes, datapb.ChannelWatchState_ToRelease)
		s.mockCluster.EXPECT().NotifyChannelOperation(mock.Anything, mock.Anything, mock.Anything).Return(nil).Twice()
		m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, s.mockAlloc)
		s.Require().NoError(err)
		s.checkAssignment(m, 1, "ch1", ToRelease)
		s.checkAssignment(m, 1, "ch2", ToRelease)

		m.AdvanceChannelState(ctx)
		s.checkAssignment(m, 1, "ch1", Releasing)
		s.checkAssignment(m, 1, "ch2", Releasing)
	})
	s.Run("advance to release channels notify fail", func() {
		chNodes := map[string]int64{
			"ch1": 1,
			"ch2": 1,
		}
		s.prepareMeta(chNodes, datapb.ChannelWatchState_ToRelease)
		s.mockCluster.EXPECT().NotifyChannelOperation(mock.Anything, mock.Anything, mock.Anything).
			Return(errors.New("mock error")).Twice()
		m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, s.mockAlloc)
		s.Require().NoError(err)
		s.checkAssignment(m, 1, "ch1", ToRelease)
		s.checkAssignment(m, 1, "ch2", ToRelease)

		m.AdvanceChannelState(ctx)
		s.checkAssignment(m, 1, "ch1", ToRelease)
		s.checkAssignment(m, 1, "ch2", ToRelease)
	})
}

func (s *ChannelManagerSuite) TestStartup() {
	chNodes := map[string]int64{
		"ch1": 1,
		"ch2": 1,
		"ch3": 3,
	}
	s.prepareMeta(chNodes, datapb.ChannelWatchState_ToRelease)
	s.mockHandler.EXPECT().CheckShouldDropChannel(mock.Anything).Return(false)
	m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, s.mockAlloc)
	s.Require().NoError(err)

	var (
		legacyNodes = []int64{1}
		allNodes    = []int64{1}
	)
	err = m.Startup(context.TODO(), legacyNodes, allNodes)
	s.NoError(err)

	s.checkAssignment(m, 1, "ch1", Legacy)
	s.checkAssignment(m, 1, "ch2", Legacy)
	s.checkAssignment(m, bufferID, "ch3", Standby)

	err = m.DeleteNode(1)
	s.NoError(err)

	s.checkAssignment(m, bufferID, "ch1", Standby)
	s.checkAssignment(m, bufferID, "ch2", Standby)

	err = m.AddNode(2)
	s.NoError(err)
	info := m.store.GetNode(2)
	s.NotNil(info)
	s.Empty(info.Channels)
	s.Equal(info.NodeID, int64(2))
}

func (s *ChannelManagerSuite) TestStartupNilSchema() {
	chNodes := map[string]int64{
		"ch1": 1,
		"ch2": 1,
		"ch3": 3,
	}
	var keys, values []string
	for channel, nodeID := range chNodes {
		keys = append(keys, fmt.Sprintf("channel_store/%d/%s", nodeID, channel))
		info := generateWatchInfo(channel, datapb.ChannelWatchState_ToRelease)
		info.Schema = nil
		bs, err := proto.Marshal(info)
		s.Require().NoError(err)
		values = append(values, string(bs))
	}
	s.mockKv.EXPECT().LoadWithPrefix(mock.Anything, mock.Anything).Return(keys, values, nil).Once()
	s.mockHandler.EXPECT().CheckShouldDropChannel(mock.Anything).Return(false)
	m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, s.mockAlloc)
	s.Require().NoError(err)
	err = m.Startup(context.TODO(), nil, []int64{1, 3})
	s.Require().NoError(err)

	for ch, node := range chNodes {
		channel, got := m.GetChannel(node, ch)
		s.Require().True(got)
		s.Nil(channel.GetSchema())
		s.Equal(ch, channel.GetName())
		log.Info("Recovered nil schema channel", zap.Any("channel", channel))
	}

	s.mockHandler.EXPECT().GetCollection(mock.Anything, mock.Anything).Return(
		&collectionInfo{ID: 111, Schema: &schemapb.CollectionSchema{Name: "coll111"}},
		nil,
	)

	err = m.DeleteNode(1)
	s.Require().NoError(err)

	err = m.DeleteNode(3)
	s.Require().NoError(err)

	s.checkAssignment(m, bufferID, "ch1", Standby)
	s.checkAssignment(m, bufferID, "ch2", Standby)
	s.checkAssignment(m, bufferID, "ch3", Standby)

	for ch := range chNodes {
		channel, got := m.GetChannel(bufferID, ch)
		s.Require().True(got)
		s.NotNil(channel.GetSchema())
		s.Equal(ch, channel.GetName())

		s.NotNil(channel.GetWatchInfo())
		s.NotNil(channel.GetWatchInfo().Schema)
		log.Info("Recovered non-nil schema channel", zap.Any("channel", channel))
	}
}

func (s *ChannelManagerSuite) TestStartupRootCoordFailed() {
	chNodes := map[string]int64{
		"ch1": 1,
		"ch2": 1,
		"ch3": 1,
		"ch4": bufferID,
	}
	s.prepareMeta(chNodes, datapb.ChannelWatchState_ToWatch)

	s.mockAlloc = globalIDAllocator.NewMockGlobalIDAllocator(s.T())
	s.mockAlloc.EXPECT().AllocOne().Return(0, errors.New("mock error")).Maybe()
	m, err := NewChannelManager(s.mockKv, s.mockHandler, s.mockCluster, s.mockAlloc)
	s.Require().NoError(err)

	err = m.Startup(context.TODO(), nil, []int64{2})
	s.Error(err)
}

func (s *ChannelManagerSuite) TestCheckLoop() {}
func (s *ChannelManagerSuite) TestGet()       {}

func (s *ChannelManagerSuite) TestGetChannelWatchInfos() {
	store := NewMockRWChannelStore(s.T())
	store.EXPECT().GetNodesChannels().Return([]*NodeChannelInfo{
		{
			NodeID: 1,
			Channels: map[string]RWChannel{
				"ch1": &channelMeta{
					WatchInfo: &datapb.ChannelWatchInfo{
						Vchan: &datapb.VchannelInfo{
							ChannelName: "ch1",
						},
						StartTs: 100,
						State:   datapb.ChannelWatchState_ToWatch,
						OpID:    1,
					},
				},
			},
		},
		{
			NodeID: 2,
			Channels: map[string]RWChannel{
				"ch2": &channelMeta{
					WatchInfo: &datapb.ChannelWatchInfo{
						Vchan: &datapb.VchannelInfo{
							ChannelName: "ch2",
						},
						StartTs: 10,
						State:   datapb.ChannelWatchState_WatchSuccess,
						OpID:    1,
					},
				},
			},
		},
	})

	cm := &ChannelManagerImpl{store: store}
	infos := cm.GetChannelWatchInfos()
	s.Equal(2, len(infos))
	s.Equal("ch1", infos[1]["ch1"].GetVchan().ChannelName)
	s.Equal("ch2", infos[2]["ch2"].GetVchan().ChannelName)

	// test empty value
	store.EXPECT().GetNodesChannels().Unset()
	store.EXPECT().GetNodesChannels().Return([]*NodeChannelInfo{})
	infos = cm.GetChannelWatchInfos()
	s.Equal(0, len(infos))
}
