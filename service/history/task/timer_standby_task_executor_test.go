// Copyright (c) 2020 Uber Technologies, Inc.
// Portions of the Software are attributed to Copyright (c) 2020 Temporal Technologies Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package task

import (
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/pborman/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/cache"
	"github.com/uber/cadence/common/clock"
	"github.com/uber/cadence/common/cluster"
	"github.com/uber/cadence/common/log"
	"github.com/uber/cadence/common/mocks"
	"github.com/uber/cadence/common/ndc"
	"github.com/uber/cadence/common/persistence"
	"github.com/uber/cadence/common/types"
	"github.com/uber/cadence/service/history/config"
	"github.com/uber/cadence/service/history/constants"
	"github.com/uber/cadence/service/history/engine"
	"github.com/uber/cadence/service/history/events"
	"github.com/uber/cadence/service/history/execution"
	"github.com/uber/cadence/service/history/shard"
	test "github.com/uber/cadence/service/history/testing"
)

type (
	timerStandbyTaskExecutorSuite struct {
		suite.Suite
		*require.Assertions

		controller             *gomock.Controller
		mockShard              *shard.TestContext
		mockEngine             *engine.MockEngine
		mockDomainCache        *cache.MockDomainCache
		mockClusterMetadata    *cluster.MockMetadata
		mockNDCHistoryResender *ndc.MockHistoryResender

		mockExecutionMgr *mocks.ExecutionManager

		logger               log.Logger
		domainID             string
		domainEntry          *cache.DomainCacheEntry
		version              int64
		clusterName          string
		now                  time.Time
		timeSource           *clock.EventTimeSource
		fetchHistoryDuration time.Duration
		discardDuration      time.Duration

		timerStandbyTaskExecutor *timerStandbyTaskExecutor
	}
)

func TestTimerStandbyTaskExecutorSuite(t *testing.T) {
	s := new(timerStandbyTaskExecutorSuite)
	suite.Run(t, s)
}

func (s *timerStandbyTaskExecutorSuite) SetupSuite() {

}

func (s *timerStandbyTaskExecutorSuite) SetupTest() {
	s.Assertions = require.New(s.T())

	config := config.NewForTest()
	s.domainID = constants.TestDomainID
	s.domainEntry = constants.TestGlobalDomainEntry
	s.version = s.domainEntry.GetFailoverVersion()
	s.clusterName = cluster.TestAlternativeClusterName
	s.now = time.Now()
	s.timeSource = clock.NewEventTimeSource().Update(s.now)
	s.fetchHistoryDuration = config.StandbyTaskMissingEventsResendDelay() +
		(config.StandbyTaskMissingEventsDiscardDelay()-config.StandbyTaskMissingEventsResendDelay())/2
	s.discardDuration = config.StandbyTaskMissingEventsDiscardDelay() * 2

	s.controller = gomock.NewController(s.T())

	s.mockShard = shard.NewTestContext(
		s.controller,
		&persistence.ShardInfo{
			RangeID:          1,
			TransferAckLevel: 0,
		},
		config,
	)
	s.mockShard.SetEventsCache(events.NewCache(
		s.mockShard.GetShardID(),
		s.mockShard.GetHistoryManager(),
		s.mockShard.GetConfig(),
		s.mockShard.GetLogger(),
		s.mockShard.GetMetricsClient(),
	))
	s.mockShard.Resource.TimeSource = s.timeSource

	s.mockEngine = engine.NewMockEngine(s.controller)
	s.mockEngine.EXPECT().NotifyNewHistoryEvent(gomock.Any()).AnyTimes()
	s.mockEngine.EXPECT().NotifyNewTransferTasks(gomock.Any(), gomock.Any()).AnyTimes()
	s.mockEngine.EXPECT().NotifyNewTimerTasks(gomock.Any(), gomock.Any()).AnyTimes()
	s.mockEngine.EXPECT().NotifyNewCrossClusterTasks(gomock.Any(), gomock.Any()).AnyTimes()
	s.mockShard.SetEngine(s.mockEngine)
	s.mockNDCHistoryResender = ndc.NewMockHistoryResender(s.controller)

	// ack manager will use the domain information
	s.mockDomainCache = s.mockShard.Resource.DomainCache
	s.mockExecutionMgr = s.mockShard.Resource.ExecutionMgr
	s.mockClusterMetadata = s.mockShard.Resource.ClusterMetadata
	s.mockDomainCache.EXPECT().GetDomainByID(gomock.Any()).Return(constants.TestGlobalDomainEntry, nil).AnyTimes()
	s.mockDomainCache.EXPECT().GetDomainName(gomock.Any()).Return(constants.TestDomainName, nil).AnyTimes()
	s.mockClusterMetadata.EXPECT().GetCurrentClusterName().Return(cluster.TestCurrentClusterName).AnyTimes()
	s.mockClusterMetadata.EXPECT().GetAllClusterInfo().Return(cluster.TestAllClusterInfo).AnyTimes()
	s.mockClusterMetadata.EXPECT().IsGlobalDomainEnabled().Return(true).AnyTimes()
	s.mockClusterMetadata.EXPECT().ClusterNameForFailoverVersion(s.version).Return(s.clusterName).AnyTimes()

	s.logger = s.mockShard.GetLogger()
	s.timerStandbyTaskExecutor = NewTimerStandbyTaskExecutor(
		s.mockShard,
		nil,
		execution.NewCache(s.mockShard),
		s.mockNDCHistoryResender,
		s.logger,
		s.mockShard.GetMetricsClient(),
		s.clusterName,
		config,
	).(*timerStandbyTaskExecutor)
}

func (s *timerStandbyTaskExecutorSuite) TearDownTest() {
	s.controller.Finish()
	s.mockShard.Finish(s.T())
}

func (s *timerStandbyTaskExecutorSuite) TestProcessUserTimerTimeout_Pending() {

	workflowExecution := types.WorkflowExecution{
		WorkflowID: "some random workflow ID",
		RunID:      uuid.New(),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	mutableState := execution.NewMutableStateBuilderWithVersionHistoriesWithEventV2(
		s.mockShard,
		s.logger,
		s.version,
		workflowExecution.GetRunID(),
		constants.TestGlobalDomainEntry,
	)
	_, err := mutableState.AddWorkflowExecutionStartedEvent(
		workflowExecution,
		&types.HistoryStartWorkflowExecutionRequest{
			DomainUUID: s.domainID,
			StartRequest: &types.StartWorkflowExecutionRequest{
				WorkflowType:                        &types.WorkflowType{Name: workflowType},
				TaskList:                            &types.TaskList{Name: taskListName},
				ExecutionStartToCloseTimeoutSeconds: common.Int32Ptr(2),
				TaskStartToCloseTimeoutSeconds:      common.Int32Ptr(1),
			},
		},
	)
	s.Nil(err)

	di := test.AddDecisionTaskScheduledEvent(mutableState)
	event := test.AddDecisionTaskStartedEvent(mutableState, di.ScheduleID, taskListName, uuid.New())
	di.StartedID = event.GetEventID()
	event = test.AddDecisionTaskCompletedEvent(mutableState, di.ScheduleID, di.StartedID, nil, "some random identity")

	timerID := "timer"
	timerTimeout := 2 * time.Second
	event, _ = test.AddTimerStartedEvent(mutableState, event.GetEventID(), timerID, int64(timerTimeout.Seconds()))
	nextEventID := event.GetEventID()

	timerSequence := execution.NewTimerSequence(s.timeSource, mutableState)
	mutableState.DeleteTimerTasks()
	modified, err := timerSequence.CreateNextUserTimer()
	s.NoError(err)
	s.True(modified)
	task := mutableState.GetTimerTasks()[0]
	timerTask := s.newTimerTaskFromInfo(&persistence.TimerTaskInfo{
		Version:             s.version,
		DomainID:            s.domainID,
		WorkflowID:          workflowExecution.GetWorkflowID(),
		RunID:               workflowExecution.GetRunID(),
		TaskID:              int64(100),
		TaskType:            persistence.TaskTypeUserTimer,
		TimeoutType:         int(types.TimeoutTypeStartToClose),
		VisibilityTimestamp: task.(*persistence.UserTimerTask).GetVisibilityTimestamp(),
		EventID:             event.GetEventID(),
	})

	persistenceMutableState := s.createPersistenceMutableState(mutableState, event.GetEventID(), event.GetVersion())
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything, mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil)

	s.mockShard.SetCurrentTime(s.clusterName, s.now)
	err = s.timerStandbyTaskExecutor.Execute(timerTask, true)
	s.Equal(ErrTaskRedispatch, err)

	s.mockShard.SetCurrentTime(s.clusterName, s.now.Add(s.fetchHistoryDuration))
	s.mockNDCHistoryResender.EXPECT().SendSingleWorkflowHistory(
		timerTask.GetDomainID(),
		timerTask.GetWorkflowID(),
		timerTask.GetRunID(),
		common.Int64Ptr(nextEventID),
		common.Int64Ptr(s.version),
		nil,
		nil,
	).Return(nil).Times(1)

	err = s.timerStandbyTaskExecutor.Execute(timerTask, true)
	s.Equal(ErrTaskRedispatch, err)

	s.mockShard.SetCurrentTime(s.clusterName, s.now.Add(s.discardDuration))
	err = s.timerStandbyTaskExecutor.Execute(timerTask, true)
	s.Equal(ErrTaskDiscarded, err)
}

func (s *timerStandbyTaskExecutorSuite) TestProcessUserTimerTimeout_Success() {

	workflowExecution := types.WorkflowExecution{
		WorkflowID: "some random workflow ID",
		RunID:      uuid.New(),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	mutableState := execution.NewMutableStateBuilderWithVersionHistoriesWithEventV2(
		s.mockShard,
		s.logger,
		s.version,
		workflowExecution.GetRunID(),
		constants.TestGlobalDomainEntry,
	)
	_, err := mutableState.AddWorkflowExecutionStartedEvent(
		workflowExecution,
		&types.HistoryStartWorkflowExecutionRequest{
			DomainUUID: s.domainID,
			StartRequest: &types.StartWorkflowExecutionRequest{
				WorkflowType:                        &types.WorkflowType{Name: workflowType},
				TaskList:                            &types.TaskList{Name: taskListName},
				ExecutionStartToCloseTimeoutSeconds: common.Int32Ptr(2),
				TaskStartToCloseTimeoutSeconds:      common.Int32Ptr(1),
			},
		},
	)
	s.Nil(err)

	di := test.AddDecisionTaskScheduledEvent(mutableState)
	event := test.AddDecisionTaskStartedEvent(mutableState, di.ScheduleID, taskListName, uuid.New())
	di.StartedID = event.GetEventID()
	event = test.AddDecisionTaskCompletedEvent(mutableState, di.ScheduleID, di.StartedID, nil, "some random identity")

	timerID := "timer"
	timerTimeout := 2 * time.Second
	event, _ = test.AddTimerStartedEvent(mutableState, event.GetEventID(), timerID, int64(timerTimeout.Seconds()))

	timerSequence := execution.NewTimerSequence(s.timeSource, mutableState)
	mutableState.DeleteTimerTasks()
	modified, err := timerSequence.CreateNextUserTimer()
	s.NoError(err)
	s.True(modified)
	task := mutableState.GetTimerTasks()[0]
	timerTask := s.newTimerTaskFromInfo(&persistence.TimerTaskInfo{
		Version:             s.version,
		DomainID:            s.domainID,
		WorkflowID:          workflowExecution.GetWorkflowID(),
		RunID:               workflowExecution.GetRunID(),
		TaskID:              int64(100),
		TaskType:            persistence.TaskTypeUserTimer,
		TimeoutType:         int(types.TimeoutTypeStartToClose),
		VisibilityTimestamp: task.(*persistence.UserTimerTask).GetVisibilityTimestamp(),
		EventID:             event.GetEventID(),
	})

	event = test.AddTimerFiredEvent(mutableState, timerID)
	mutableState.FlushBufferedEvents()

	persistenceMutableState := s.createPersistenceMutableState(mutableState, event.GetEventID(), event.GetVersion())
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything, mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()

	s.mockShard.SetCurrentTime(s.clusterName, s.now)
	err = s.timerStandbyTaskExecutor.Execute(timerTask, true)
	s.Nil(err)
}

func (s *timerStandbyTaskExecutorSuite) TestProcessUserTimerTimeout_Multiple() {

	workflowExecution := types.WorkflowExecution{
		WorkflowID: "some random workflow ID",
		RunID:      uuid.New(),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	mutableState := execution.NewMutableStateBuilderWithVersionHistoriesWithEventV2(
		s.mockShard,
		s.logger,
		s.version,
		workflowExecution.GetRunID(),
		constants.TestGlobalDomainEntry,
	)
	_, err := mutableState.AddWorkflowExecutionStartedEvent(
		workflowExecution,
		&types.HistoryStartWorkflowExecutionRequest{
			DomainUUID: s.domainID,
			StartRequest: &types.StartWorkflowExecutionRequest{
				WorkflowType:                        &types.WorkflowType{Name: workflowType},
				TaskList:                            &types.TaskList{Name: taskListName},
				ExecutionStartToCloseTimeoutSeconds: common.Int32Ptr(2),
				TaskStartToCloseTimeoutSeconds:      common.Int32Ptr(1),
			},
		},
	)
	s.Nil(err)

	di := test.AddDecisionTaskScheduledEvent(mutableState)
	event := test.AddDecisionTaskStartedEvent(mutableState, di.ScheduleID, taskListName, uuid.New())
	di.StartedID = event.GetEventID()
	event = test.AddDecisionTaskCompletedEvent(mutableState, di.ScheduleID, di.StartedID, nil, "some random identity")

	timerID1 := "timer-1"
	timerTimeout1 := 2 * time.Second
	event, _ = test.AddTimerStartedEvent(mutableState, event.GetEventID(), timerID1, int64(timerTimeout1.Seconds()))

	timerID2 := "timer-2"
	timerTimeout2 := 50 * time.Second
	_, _ = test.AddTimerStartedEvent(mutableState, event.GetEventID(), timerID2, int64(timerTimeout2.Seconds()))

	timerSequence := execution.NewTimerSequence(s.timeSource, mutableState)
	mutableState.DeleteTimerTasks()
	modified, err := timerSequence.CreateNextUserTimer()
	s.NoError(err)
	s.True(modified)
	task := mutableState.GetTimerTasks()[0]
	timerTask := s.newTimerTaskFromInfo(&persistence.TimerTaskInfo{
		Version:             s.version,
		DomainID:            s.domainID,
		WorkflowID:          workflowExecution.GetWorkflowID(),
		RunID:               workflowExecution.GetRunID(),
		TaskID:              int64(100),
		TaskType:            persistence.TaskTypeUserTimer,
		TimeoutType:         int(types.TimeoutTypeStartToClose),
		VisibilityTimestamp: task.(*persistence.UserTimerTask).GetVisibilityTimestamp(),
		EventID:             event.GetEventID(),
	})

	event = test.AddTimerFiredEvent(mutableState, timerID1)
	mutableState.FlushBufferedEvents()

	persistenceMutableState := s.createPersistenceMutableState(mutableState, event.GetEventID(), event.GetVersion())
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything, mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()

	s.mockShard.SetCurrentTime(s.clusterName, s.now)
	err = s.timerStandbyTaskExecutor.Execute(timerTask, true)
	s.Nil(err)
}

func (s *timerStandbyTaskExecutorSuite) TestProcessActivityTimeout_Pending() {

	workflowExecution := types.WorkflowExecution{
		WorkflowID: "some random workflow ID",
		RunID:      uuid.New(),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	mutableState := execution.NewMutableStateBuilderWithVersionHistoriesWithEventV2(
		s.mockShard,
		s.logger,
		s.version,
		workflowExecution.GetRunID(),
		constants.TestGlobalDomainEntry,
	)
	_, err := mutableState.AddWorkflowExecutionStartedEvent(
		workflowExecution,
		&types.HistoryStartWorkflowExecutionRequest{
			DomainUUID: s.domainID,
			StartRequest: &types.StartWorkflowExecutionRequest{
				WorkflowType:                        &types.WorkflowType{Name: workflowType},
				TaskList:                            &types.TaskList{Name: taskListName},
				ExecutionStartToCloseTimeoutSeconds: common.Int32Ptr(2),
				TaskStartToCloseTimeoutSeconds:      common.Int32Ptr(1),
			},
		},
	)
	s.Nil(err)

	di := test.AddDecisionTaskScheduledEvent(mutableState)
	event := test.AddDecisionTaskStartedEvent(mutableState, di.ScheduleID, taskListName, uuid.New())
	di.StartedID = event.GetEventID()
	event = test.AddDecisionTaskCompletedEvent(mutableState, di.ScheduleID, di.StartedID, nil, "some random identity")

	tasklist := "tasklist"
	activityID := "activity"
	activityType := "activity type"
	timerTimeout := 2 * time.Second
	scheduledEvent, _ := test.AddActivityTaskScheduledEvent(mutableState, event.GetEventID(), activityID, activityType, tasklist, []byte(nil),
		int32(timerTimeout.Seconds()), int32(timerTimeout.Seconds()), int32(timerTimeout.Seconds()), int32(timerTimeout.Seconds()))
	nextEventID := scheduledEvent.GetEventID()

	timerSequence := execution.NewTimerSequence(s.timeSource, mutableState)
	mutableState.DeleteTimerTasks()
	modified, err := timerSequence.CreateNextActivityTimer()
	s.NoError(err)
	s.True(modified)
	task := mutableState.GetTimerTasks()[0]
	timerTask := s.newTimerTaskFromInfo(&persistence.TimerTaskInfo{
		Version:             s.version,
		DomainID:            s.domainID,
		WorkflowID:          workflowExecution.GetWorkflowID(),
		RunID:               workflowExecution.GetRunID(),
		TaskID:              int64(100),
		TaskType:            persistence.TaskTypeActivityTimeout,
		TimeoutType:         int(types.TimeoutTypeScheduleToClose),
		VisibilityTimestamp: task.(*persistence.ActivityTimeoutTask).GetVisibilityTimestamp(),
		EventID:             scheduledEvent.GetEventID(),
	})

	persistenceMutableState := s.createPersistenceMutableState(mutableState, scheduledEvent.GetEventID(), scheduledEvent.GetVersion())
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything, mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()

	s.mockShard.SetCurrentTime(s.clusterName, s.now)
	err = s.timerStandbyTaskExecutor.Execute(timerTask, true)
	s.Equal(ErrTaskRedispatch, err)

	s.mockShard.SetCurrentTime(s.clusterName, s.now.Add(s.fetchHistoryDuration))
	s.mockNDCHistoryResender.EXPECT().SendSingleWorkflowHistory(
		timerTask.GetDomainID(),
		timerTask.GetWorkflowID(),
		timerTask.GetRunID(),
		common.Int64Ptr(nextEventID),
		common.Int64Ptr(s.version),
		nil,
		nil,
	).Return(nil).Times(1)
	err = s.timerStandbyTaskExecutor.Execute(timerTask, true)
	s.Equal(ErrTaskRedispatch, err)

	s.mockShard.SetCurrentTime(s.clusterName, s.now.Add(s.discardDuration))
	err = s.timerStandbyTaskExecutor.Execute(timerTask, true)
	s.Equal(ErrTaskDiscarded, err)
}

func (s *timerStandbyTaskExecutorSuite) TestProcessActivityTimeout_Success() {

	workflowExecution := types.WorkflowExecution{
		WorkflowID: "some random workflow ID",
		RunID:      uuid.New(),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	mutableState := execution.NewMutableStateBuilderWithVersionHistoriesWithEventV2(
		s.mockShard,
		s.logger,
		s.version,
		workflowExecution.GetRunID(),
		constants.TestGlobalDomainEntry,
	)
	_, err := mutableState.AddWorkflowExecutionStartedEvent(
		workflowExecution,
		&types.HistoryStartWorkflowExecutionRequest{
			DomainUUID: s.domainID,
			StartRequest: &types.StartWorkflowExecutionRequest{
				WorkflowType:                        &types.WorkflowType{Name: workflowType},
				TaskList:                            &types.TaskList{Name: taskListName},
				ExecutionStartToCloseTimeoutSeconds: common.Int32Ptr(2),
				TaskStartToCloseTimeoutSeconds:      common.Int32Ptr(1),
			},
		},
	)
	s.Nil(err)

	di := test.AddDecisionTaskScheduledEvent(mutableState)
	event := test.AddDecisionTaskStartedEvent(mutableState, di.ScheduleID, taskListName, uuid.New())
	di.StartedID = event.GetEventID()
	event = test.AddDecisionTaskCompletedEvent(mutableState, di.ScheduleID, di.StartedID, nil, "some random identity")

	identity := "identity"
	tasklist := "tasklist"
	activityID := "activity"
	activityType := "activity type"
	timerTimeout := 2 * time.Second
	scheduledEvent, _ := test.AddActivityTaskScheduledEvent(mutableState, event.GetEventID(), activityID, activityType, tasklist, []byte(nil),
		int32(timerTimeout.Seconds()), int32(timerTimeout.Seconds()), int32(timerTimeout.Seconds()), int32(timerTimeout.Seconds()))
	startedEvent := test.AddActivityTaskStartedEvent(mutableState, scheduledEvent.GetEventID(), identity)

	timerSequence := execution.NewTimerSequence(s.timeSource, mutableState)
	mutableState.DeleteTimerTasks()
	modified, err := timerSequence.CreateNextActivityTimer()
	s.NoError(err)
	s.True(modified)
	task := mutableState.GetTimerTasks()[0]
	timerTask := s.newTimerTaskFromInfo(&persistence.TimerTaskInfo{
		Version:             s.version,
		DomainID:            s.domainID,
		WorkflowID:          workflowExecution.GetWorkflowID(),
		RunID:               workflowExecution.GetRunID(),
		TaskID:              int64(100),
		TaskType:            persistence.TaskTypeActivityTimeout,
		TimeoutType:         int(types.TimeoutTypeScheduleToClose),
		VisibilityTimestamp: task.(*persistence.ActivityTimeoutTask).GetVisibilityTimestamp(),
		EventID:             scheduledEvent.GetEventID(),
	})

	completeEvent := test.AddActivityTaskCompletedEvent(mutableState, scheduledEvent.GetEventID(), startedEvent.GetEventID(), []byte(nil), identity)
	mutableState.FlushBufferedEvents()

	persistenceMutableState := s.createPersistenceMutableState(mutableState, completeEvent.GetEventID(), completeEvent.GetVersion())
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything, mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()

	s.mockShard.SetCurrentTime(s.clusterName, s.now)
	err = s.timerStandbyTaskExecutor.Execute(timerTask, true)
	s.Nil(err)
}

func (s *timerStandbyTaskExecutorSuite) TestProcessActivityTimeout_Heartbeat_Noop() {
	workflowExecution := types.WorkflowExecution{
		WorkflowID: "some random workflow ID",
		RunID:      uuid.New(),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	mutableState := execution.NewMutableStateBuilderWithVersionHistoriesWithEventV2(
		s.mockShard,
		s.logger,
		s.version,
		workflowExecution.GetRunID(),
		constants.TestGlobalDomainEntry,
	)
	_, err := mutableState.AddWorkflowExecutionStartedEvent(
		workflowExecution,
		&types.HistoryStartWorkflowExecutionRequest{
			DomainUUID: s.domainID,
			StartRequest: &types.StartWorkflowExecutionRequest{
				WorkflowType:                        &types.WorkflowType{Name: workflowType},
				TaskList:                            &types.TaskList{Name: taskListName},
				ExecutionStartToCloseTimeoutSeconds: common.Int32Ptr(2),
				TaskStartToCloseTimeoutSeconds:      common.Int32Ptr(1),
			},
		},
	)
	s.Nil(err)

	di := test.AddDecisionTaskScheduledEvent(mutableState)
	event := test.AddDecisionTaskStartedEvent(mutableState, di.ScheduleID, taskListName, uuid.New())
	di.StartedID = event.GetEventID()
	event = test.AddDecisionTaskCompletedEvent(mutableState, di.ScheduleID, di.StartedID, nil, "some random identity")

	identity := "identity"
	tasklist := "tasklist"
	activityID := "activity"
	activityType := "activity type"
	timerTimeout := 2 * time.Second
	heartbeatTimerTimeout := time.Second
	scheduledEvent, _ := test.AddActivityTaskScheduledEvent(mutableState, event.GetEventID(), activityID, activityType, tasklist, []byte(nil),
		int32(timerTimeout.Seconds()), int32(timerTimeout.Seconds()), int32(timerTimeout.Seconds()), int32(heartbeatTimerTimeout.Seconds()))
	startedEvent := test.AddActivityTaskStartedEvent(mutableState, scheduledEvent.GetEventID(), identity)
	mutableState.FlushBufferedEvents()

	timerSequence := execution.NewTimerSequence(s.timeSource, mutableState)
	mutableState.DeleteTimerTasks()
	modified, err := timerSequence.CreateNextActivityTimer()
	s.NoError(err)
	s.True(modified)
	task := mutableState.GetTimerTasks()[0]
	s.Equal(int(execution.TimerTypeHeartbeat), task.(*persistence.ActivityTimeoutTask).TimeoutType)
	timerTask := s.newTimerTaskFromInfo(&persistence.TimerTaskInfo{
		Version:             s.version,
		DomainID:            s.domainID,
		WorkflowID:          workflowExecution.GetWorkflowID(),
		RunID:               workflowExecution.GetRunID(),
		TaskID:              int64(100),
		TaskType:            persistence.TaskTypeActivityTimeout,
		TimeoutType:         int(types.TimeoutTypeHeartbeat),
		VisibilityTimestamp: task.(*persistence.ActivityTimeoutTask).GetVisibilityTimestamp().Add(-time.Second),
		EventID:             scheduledEvent.GetEventID(),
	})

	persistenceMutableState := s.createPersistenceMutableState(mutableState, startedEvent.GetEventID(), startedEvent.GetVersion())
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything, mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()

	s.mockShard.SetCurrentTime(s.clusterName, s.now)
	err = s.timerStandbyTaskExecutor.Execute(timerTask, true)
	s.Nil(err)
}

func (s *timerStandbyTaskExecutorSuite) TestProcessActivityTimeout_Multiple_CanUpdate() {

	workflowExecution := types.WorkflowExecution{
		WorkflowID: "some random workflow ID",
		RunID:      uuid.New(),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	mutableState := execution.NewMutableStateBuilderWithVersionHistoriesWithEventV2(
		s.mockShard,
		s.logger,
		s.version,
		workflowExecution.GetRunID(),
		constants.TestGlobalDomainEntry,
	)
	_, err := mutableState.AddWorkflowExecutionStartedEvent(
		workflowExecution,
		&types.HistoryStartWorkflowExecutionRequest{
			DomainUUID: s.domainID,
			StartRequest: &types.StartWorkflowExecutionRequest{
				WorkflowType:                        &types.WorkflowType{Name: workflowType},
				TaskList:                            &types.TaskList{Name: taskListName},
				ExecutionStartToCloseTimeoutSeconds: common.Int32Ptr(2),
				TaskStartToCloseTimeoutSeconds:      common.Int32Ptr(1),
			},
		},
	)
	s.Nil(err)

	di := test.AddDecisionTaskScheduledEvent(mutableState)
	event := test.AddDecisionTaskStartedEvent(mutableState, di.ScheduleID, taskListName, uuid.New())
	di.StartedID = event.GetEventID()
	event = test.AddDecisionTaskCompletedEvent(mutableState, di.ScheduleID, di.StartedID, nil, "some random identity")

	identity := "identity"
	tasklist := "tasklist"
	activityID1 := "activity 1"
	activityType1 := "activity type 1"
	timerTimeout1 := 2 * time.Second
	scheduledEvent1, _ := test.AddActivityTaskScheduledEvent(mutableState, event.GetEventID(), activityID1, activityType1, tasklist, []byte(nil),
		int32(timerTimeout1.Seconds()), int32(timerTimeout1.Seconds()), int32(timerTimeout1.Seconds()), int32(timerTimeout1.Seconds()))
	startedEvent1 := test.AddActivityTaskStartedEvent(mutableState, scheduledEvent1.GetEventID(), identity)

	activityID2 := "activity 2"
	activityType2 := "activity type 2"
	timerTimeout2 := 20 * time.Second
	scheduledEvent2, _ := test.AddActivityTaskScheduledEvent(mutableState, event.GetEventID(), activityID2, activityType2, tasklist, []byte(nil),
		int32(timerTimeout2.Seconds()), int32(timerTimeout2.Seconds()), int32(timerTimeout2.Seconds()), int32(timerTimeout2.Seconds()))
	test.AddActivityTaskStartedEvent(mutableState, scheduledEvent2.GetEventID(), identity)
	activityInfo2 := mutableState.GetPendingActivityInfos()[scheduledEvent2.GetEventID()]
	activityInfo2.TimerTaskStatus |= execution.TimerTaskStatusCreatedHeartbeat
	activityInfo2.LastHeartBeatUpdatedTime = time.Now()

	timerSequence := execution.NewTimerSequence(s.timeSource, mutableState)
	mutableState.DeleteTimerTasks()
	modified, err := timerSequence.CreateNextActivityTimer()
	s.NoError(err)
	s.True(modified)
	timerTask := s.newTimerTaskFromInfo(&persistence.TimerTaskInfo{
		Version:             s.version,
		DomainID:            s.domainID,
		WorkflowID:          workflowExecution.GetWorkflowID(),
		RunID:               workflowExecution.GetRunID(),
		TaskID:              int64(100),
		TaskType:            persistence.TaskTypeActivityTimeout,
		TimeoutType:         int(types.TimeoutTypeHeartbeat),
		VisibilityTimestamp: activityInfo2.LastHeartBeatUpdatedTime.Add(-5 * time.Second),
		EventID:             scheduledEvent2.GetEventID(),
	})

	completeEvent1 := test.AddActivityTaskCompletedEvent(mutableState, scheduledEvent1.GetEventID(), startedEvent1.GetEventID(), []byte(nil), identity)
	mutableState.FlushBufferedEvents()

	persistenceMutableState := s.createPersistenceMutableState(mutableState, completeEvent1.GetEventID(), completeEvent1.GetVersion())
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything, mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()
	s.mockExecutionMgr.On("UpdateWorkflowExecution", mock.Anything, mock.MatchedBy(func(input *persistence.UpdateWorkflowExecutionRequest) bool {
		s.Equal(1, len(input.UpdateWorkflowMutation.TimerTasks))
		s.Equal(1, len(input.UpdateWorkflowMutation.UpsertActivityInfos))
		mutableState.GetExecutionInfo().LastUpdatedTimestamp = input.UpdateWorkflowMutation.ExecutionInfo.LastUpdatedTimestamp
		input.RangeID = 0
		input.UpdateWorkflowMutation.ExecutionInfo.LastEventTaskID = 0
		mutableState.GetExecutionInfo().LastEventTaskID = 0
		mutableState.GetExecutionInfo().DecisionOriginalScheduledTimestamp = input.UpdateWorkflowMutation.ExecutionInfo.DecisionOriginalScheduledTimestamp
		s.Equal(&persistence.UpdateWorkflowExecutionRequest{
			UpdateWorkflowMutation: persistence.WorkflowMutation{
				ExecutionInfo:             mutableState.GetExecutionInfo(),
				ExecutionStats:            &persistence.ExecutionStats{},
				TransferTasks:             nil,
				ReplicationTasks:          nil,
				TimerTasks:                input.UpdateWorkflowMutation.TimerTasks,
				Condition:                 mutableState.GetNextEventID(),
				UpsertActivityInfos:       input.UpdateWorkflowMutation.UpsertActivityInfos,
				DeleteActivityInfos:       []int64{},
				UpsertTimerInfos:          []*persistence.TimerInfo{},
				DeleteTimerInfos:          []string{},
				UpsertChildExecutionInfos: []*persistence.ChildExecutionInfo{},
				DeleteChildExecutionInfos: []int64{},
				UpsertRequestCancelInfos:  []*persistence.RequestCancelInfo{},
				DeleteRequestCancelInfos:  []int64{},
				UpsertSignalInfos:         []*persistence.SignalInfo{},
				DeleteSignalInfos:         []int64{},
				UpsertSignalRequestedIDs:  []string{},
				DeleteSignalRequestedIDs:  []string{},
				NewBufferedEvents:         nil,
				ClearBufferedEvents:       false,
				VersionHistories:          mutableState.GetVersionHistories(),
			},
			NewWorkflowSnapshot: nil,
			Encoding:            common.EncodingType(s.mockShard.GetConfig().EventEncodingType(s.domainID)),
		}, input)
		return true
	})).Return(&persistence.UpdateWorkflowExecutionResponse{MutableStateUpdateSessionStats: &persistence.MutableStateUpdateSessionStats{}}, nil).Once()

	s.mockShard.SetCurrentTime(s.clusterName, s.now)
	err = s.timerStandbyTaskExecutor.Execute(timerTask, true)
	s.Nil(err)
}

func (s *timerStandbyTaskExecutorSuite) TestProcessDecisionTimeout_Pending() {

	workflowExecution := types.WorkflowExecution{
		WorkflowID: "some random workflow ID",
		RunID:      uuid.New(),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	mutableState := execution.NewMutableStateBuilderWithVersionHistoriesWithEventV2(
		s.mockShard,
		s.logger,
		s.version,
		workflowExecution.GetRunID(),
		constants.TestGlobalDomainEntry,
	)
	_, err := mutableState.AddWorkflowExecutionStartedEvent(
		workflowExecution,
		&types.HistoryStartWorkflowExecutionRequest{
			DomainUUID: s.domainID,
			StartRequest: &types.StartWorkflowExecutionRequest{
				WorkflowType:                        &types.WorkflowType{Name: workflowType},
				TaskList:                            &types.TaskList{Name: taskListName},
				ExecutionStartToCloseTimeoutSeconds: common.Int32Ptr(2),
				TaskStartToCloseTimeoutSeconds:      common.Int32Ptr(1),
			},
		},
	)
	s.Nil(err)

	di := test.AddDecisionTaskScheduledEvent(mutableState)
	startedEvent := test.AddDecisionTaskStartedEvent(mutableState, di.ScheduleID, taskListName, uuid.New())
	nextEventID := startedEvent.GetEventID()

	timerTask := s.newTimerTaskFromInfo(&persistence.TimerTaskInfo{
		Version:             s.version,
		DomainID:            s.domainID,
		WorkflowID:          workflowExecution.GetWorkflowID(),
		RunID:               workflowExecution.GetRunID(),
		TaskID:              int64(100),
		TaskType:            persistence.TaskTypeDecisionTimeout,
		TimeoutType:         int(types.TimeoutTypeStartToClose),
		VisibilityTimestamp: s.now,
		EventID:             di.ScheduleID,
	})

	persistenceMutableState := s.createPersistenceMutableState(mutableState, startedEvent.GetEventID(), startedEvent.GetVersion())
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything, mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()

	s.mockShard.SetCurrentTime(s.clusterName, s.now)
	err = s.timerStandbyTaskExecutor.Execute(timerTask, true)
	s.Equal(ErrTaskRedispatch, err)

	s.mockShard.SetCurrentTime(s.clusterName, s.now.Add(s.fetchHistoryDuration))
	s.mockNDCHistoryResender.EXPECT().SendSingleWorkflowHistory(
		timerTask.GetDomainID(),
		timerTask.GetWorkflowID(),
		timerTask.GetRunID(),
		common.Int64Ptr(nextEventID),
		common.Int64Ptr(s.version),
		nil,
		nil,
	).Return(nil).Times(1)
	err = s.timerStandbyTaskExecutor.Execute(timerTask, true)
	s.Equal(ErrTaskRedispatch, err)

	s.mockShard.SetCurrentTime(s.clusterName, s.now.Add(s.discardDuration))
	err = s.timerStandbyTaskExecutor.Execute(timerTask, true)
	s.Equal(ErrTaskDiscarded, err)
}

func (s *timerStandbyTaskExecutorSuite) TestProcessDecisionTimeout_ScheduleToStartTimer() {

	execution := types.WorkflowExecution{
		WorkflowID: "some random workflow ID",
		RunID:      uuid.New(),
	}

	decisionScheduleID := int64(16384)

	timerTask := s.newTimerTaskFromInfo(&persistence.TimerTaskInfo{
		Version:             s.version,
		DomainID:            s.domainID,
		WorkflowID:          execution.GetWorkflowID(),
		RunID:               execution.GetRunID(),
		TaskID:              int64(100),
		TaskType:            persistence.TaskTypeDecisionTimeout,
		TimeoutType:         int(types.TimeoutTypeScheduleToStart),
		VisibilityTimestamp: s.now,
		EventID:             decisionScheduleID,
	})

	s.mockShard.SetCurrentTime(s.clusterName, s.now)
	err := s.timerStandbyTaskExecutor.Execute(timerTask, true)
	s.Equal(nil, err)
}

func (s *timerStandbyTaskExecutorSuite) TestProcessDecisionTimeout_Success() {

	workflowExecution := types.WorkflowExecution{
		WorkflowID: "some random workflow ID",
		RunID:      uuid.New(),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	mutableState := execution.NewMutableStateBuilderWithVersionHistoriesWithEventV2(
		s.mockShard,
		s.logger,
		s.version,
		workflowExecution.GetRunID(),
		constants.TestGlobalDomainEntry,
	)
	_, err := mutableState.AddWorkflowExecutionStartedEvent(
		workflowExecution,
		&types.HistoryStartWorkflowExecutionRequest{
			DomainUUID: s.domainID,
			StartRequest: &types.StartWorkflowExecutionRequest{
				WorkflowType:                        &types.WorkflowType{Name: workflowType},
				TaskList:                            &types.TaskList{Name: taskListName},
				ExecutionStartToCloseTimeoutSeconds: common.Int32Ptr(2),
				TaskStartToCloseTimeoutSeconds:      common.Int32Ptr(1),
			},
		},
	)
	s.Nil(err)

	di := test.AddDecisionTaskScheduledEvent(mutableState)
	event := test.AddDecisionTaskStartedEvent(mutableState, di.ScheduleID, taskListName, uuid.New())
	di.StartedID = event.GetEventID()
	event = test.AddDecisionTaskCompletedEvent(mutableState, di.ScheduleID, di.StartedID, nil, "some random identity")

	timerTask := s.newTimerTaskFromInfo(&persistence.TimerTaskInfo{
		Version:             s.version,
		DomainID:            s.domainID,
		WorkflowID:          workflowExecution.GetWorkflowID(),
		RunID:               workflowExecution.GetRunID(),
		TaskID:              int64(100),
		TaskType:            persistence.TaskTypeDecisionTimeout,
		TimeoutType:         int(types.TimeoutTypeStartToClose),
		VisibilityTimestamp: s.now,
		EventID:             di.ScheduleID,
	})

	persistenceMutableState := s.createPersistenceMutableState(mutableState, event.GetEventID(), event.GetVersion())
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything, mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()

	s.mockShard.SetCurrentTime(s.clusterName, s.now)
	err = s.timerStandbyTaskExecutor.Execute(timerTask, true)
	s.Nil(err)
}

func (s *timerStandbyTaskExecutorSuite) TestProcessWorkflowBackoffTimer_Pending() {

	workflowExecution := types.WorkflowExecution{
		WorkflowID: "some random workflow ID",
		RunID:      uuid.New(),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	mutableState := execution.NewMutableStateBuilderWithVersionHistoriesWithEventV2(
		s.mockShard,
		s.logger,
		s.version,
		workflowExecution.GetRunID(),
		constants.TestGlobalDomainEntry,
	)
	event, err := mutableState.AddWorkflowExecutionStartedEvent(
		workflowExecution,
		&types.HistoryStartWorkflowExecutionRequest{
			DomainUUID: s.domainID,
			StartRequest: &types.StartWorkflowExecutionRequest{
				WorkflowType:                        &types.WorkflowType{Name: workflowType},
				TaskList:                            &types.TaskList{Name: taskListName},
				ExecutionStartToCloseTimeoutSeconds: common.Int32Ptr(2),
				TaskStartToCloseTimeoutSeconds:      common.Int32Ptr(1),
			},
		},
	)
	s.Nil(err)
	mutableState.FlushBufferedEvents()
	nextEventID := event.GetEventID()

	timerTask := s.newTimerTaskFromInfo(&persistence.TimerTaskInfo{
		Version:             s.version,
		DomainID:            s.domainID,
		WorkflowID:          workflowExecution.GetWorkflowID(),
		RunID:               workflowExecution.GetRunID(),
		TaskID:              int64(100),
		TaskType:            persistence.TaskTypeWorkflowBackoffTimer,
		VisibilityTimestamp: s.now,
	})

	persistenceMutableState := s.createPersistenceMutableState(mutableState, event.GetEventID(), event.GetVersion())
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything, mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()

	s.mockShard.SetCurrentTime(s.clusterName, s.now)
	err = s.timerStandbyTaskExecutor.Execute(timerTask, true)
	s.Equal(ErrTaskRedispatch, err)

	s.mockShard.SetCurrentTime(s.clusterName, time.Now().Add(s.fetchHistoryDuration))
	s.mockNDCHistoryResender.EXPECT().SendSingleWorkflowHistory(
		timerTask.GetDomainID(),
		timerTask.GetWorkflowID(),
		timerTask.GetRunID(),
		common.Int64Ptr(nextEventID),
		common.Int64Ptr(s.version),
		nil,
		nil,
	).Return(nil).Times(1)
	err = s.timerStandbyTaskExecutor.Execute(timerTask, true)
	s.Equal(ErrTaskRedispatch, err)

	s.mockShard.SetCurrentTime(s.clusterName, time.Now().Add(s.discardDuration))
	err = s.timerStandbyTaskExecutor.Execute(timerTask, true)
	s.Equal(ErrTaskDiscarded, err)
}

func (s *timerStandbyTaskExecutorSuite) TestProcessWorkflowBackoffTimer_Success() {

	workflowExecution := types.WorkflowExecution{
		WorkflowID: "some random workflow ID",
		RunID:      uuid.New(),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	mutableState := execution.NewMutableStateBuilderWithVersionHistoriesWithEventV2(
		s.mockShard,
		s.logger,
		s.version,
		workflowExecution.GetRunID(),
		constants.TestGlobalDomainEntry,
	)
	_, err := mutableState.AddWorkflowExecutionStartedEvent(
		workflowExecution,
		&types.HistoryStartWorkflowExecutionRequest{
			DomainUUID: s.domainID,
			StartRequest: &types.StartWorkflowExecutionRequest{
				WorkflowType:                        &types.WorkflowType{Name: workflowType},
				TaskList:                            &types.TaskList{Name: taskListName},
				ExecutionStartToCloseTimeoutSeconds: common.Int32Ptr(2),
				TaskStartToCloseTimeoutSeconds:      common.Int32Ptr(1),
			},
		},
	)
	s.Nil(err)

	di := test.AddDecisionTaskScheduledEvent(mutableState)

	timerTask := s.newTimerTaskFromInfo(&persistence.TimerTaskInfo{
		Version:             s.version,
		DomainID:            s.domainID,
		WorkflowID:          workflowExecution.GetWorkflowID(),
		RunID:               workflowExecution.GetRunID(),
		TaskID:              int64(100),
		TaskType:            persistence.TaskTypeWorkflowBackoffTimer,
		VisibilityTimestamp: s.now,
	})

	persistenceMutableState := s.createPersistenceMutableState(mutableState, di.ScheduleID, di.Version)
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything, mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()

	s.mockShard.SetCurrentTime(s.clusterName, s.now)
	err = s.timerStandbyTaskExecutor.Execute(timerTask, true)
	s.Nil(err)
}

func (s *timerStandbyTaskExecutorSuite) TestProcessWorkflowTimeout_Pending() {

	workflowExecution := types.WorkflowExecution{
		WorkflowID: "some random workflow ID",
		RunID:      uuid.New(),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	mutableState := execution.NewMutableStateBuilderWithVersionHistoriesWithEventV2(
		s.mockShard,
		s.logger,
		s.version,
		workflowExecution.GetRunID(),
		constants.TestGlobalDomainEntry,
	)
	_, err := mutableState.AddWorkflowExecutionStartedEvent(
		workflowExecution,
		&types.HistoryStartWorkflowExecutionRequest{
			DomainUUID: s.domainID,
			StartRequest: &types.StartWorkflowExecutionRequest{
				WorkflowType:                        &types.WorkflowType{Name: workflowType},
				TaskList:                            &types.TaskList{Name: taskListName},
				ExecutionStartToCloseTimeoutSeconds: common.Int32Ptr(2),
				TaskStartToCloseTimeoutSeconds:      common.Int32Ptr(1),
			},
		},
	)
	s.Nil(err)

	di := test.AddDecisionTaskScheduledEvent(mutableState)
	startEvent := test.AddDecisionTaskStartedEvent(mutableState, di.ScheduleID, taskListName, uuid.New())
	di.StartedID = startEvent.GetEventID()
	completionEvent := test.AddDecisionTaskCompletedEvent(mutableState, di.ScheduleID, di.StartedID, nil, "some random identity")
	mutableState.FlushBufferedEvents()
	nextEventID := completionEvent.GetEventID()

	timerTask := s.newTimerTaskFromInfo(&persistence.TimerTaskInfo{
		Version:             s.version,
		DomainID:            s.domainID,
		WorkflowID:          workflowExecution.GetWorkflowID(),
		RunID:               workflowExecution.GetRunID(),
		TaskID:              int64(100),
		TaskType:            persistence.TaskTypeWorkflowTimeout,
		TimeoutType:         int(types.TimeoutTypeStartToClose),
		VisibilityTimestamp: s.now,
	})

	persistenceMutableState := s.createPersistenceMutableState(mutableState, completionEvent.GetEventID(), completionEvent.GetVersion())
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything, mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()

	s.mockShard.SetCurrentTime(s.clusterName, s.now)
	err = s.timerStandbyTaskExecutor.Execute(timerTask, true)
	s.Equal(ErrTaskRedispatch, err)

	s.mockShard.SetCurrentTime(s.clusterName, s.now.Add(s.fetchHistoryDuration))
	s.mockNDCHistoryResender.EXPECT().SendSingleWorkflowHistory(
		timerTask.GetDomainID(),
		timerTask.GetWorkflowID(),
		timerTask.GetRunID(),
		common.Int64Ptr(nextEventID),
		common.Int64Ptr(s.version),
		nil,
		nil,
	).Return(nil).Times(1)
	err = s.timerStandbyTaskExecutor.Execute(timerTask, true)
	s.Equal(ErrTaskRedispatch, err)

	s.mockShard.SetCurrentTime(s.clusterName, s.now.Add(s.discardDuration))
	err = s.timerStandbyTaskExecutor.Execute(timerTask, true)
	s.Equal(ErrTaskDiscarded, err)
}

func (s *timerStandbyTaskExecutorSuite) TestProcessWorkflowTimeout_Success() {

	workflowExecution := types.WorkflowExecution{
		WorkflowID: "some random workflow ID",
		RunID:      uuid.New(),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	mutableState := execution.NewMutableStateBuilderWithVersionHistoriesWithEventV2(
		s.mockShard,
		s.logger,
		s.version,
		workflowExecution.GetRunID(),
		constants.TestGlobalDomainEntry,
	)
	_, err := mutableState.AddWorkflowExecutionStartedEvent(
		workflowExecution,
		&types.HistoryStartWorkflowExecutionRequest{
			DomainUUID: s.domainID,
			StartRequest: &types.StartWorkflowExecutionRequest{
				WorkflowType:                        &types.WorkflowType{Name: workflowType},
				TaskList:                            &types.TaskList{Name: taskListName},
				ExecutionStartToCloseTimeoutSeconds: common.Int32Ptr(2),
				TaskStartToCloseTimeoutSeconds:      common.Int32Ptr(1),
			},
		},
	)
	s.Nil(err)

	di := test.AddDecisionTaskScheduledEvent(mutableState)
	event := test.AddDecisionTaskStartedEvent(mutableState, di.ScheduleID, taskListName, uuid.New())
	di.StartedID = event.GetEventID()
	event = test.AddDecisionTaskCompletedEvent(mutableState, di.ScheduleID, di.StartedID, nil, "some random identity")
	event = test.AddCompleteWorkflowEvent(mutableState, event.GetEventID(), nil)

	timerTask := s.newTimerTaskFromInfo(&persistence.TimerTaskInfo{
		Version:             s.version,
		DomainID:            s.domainID,
		WorkflowID:          workflowExecution.GetWorkflowID(),
		RunID:               workflowExecution.GetRunID(),
		TaskID:              int64(100),
		TaskType:            persistence.TaskTypeWorkflowTimeout,
		TimeoutType:         int(types.TimeoutTypeStartToClose),
		VisibilityTimestamp: s.now,
	})

	persistenceMutableState := s.createPersistenceMutableState(mutableState, event.GetEventID(), event.GetVersion())
	s.mockExecutionMgr.On("GetWorkflowExecution", mock.Anything, mock.Anything).Return(&persistence.GetWorkflowExecutionResponse{State: persistenceMutableState}, nil).Once()

	s.mockShard.SetCurrentTime(s.clusterName, s.now)
	err = s.timerStandbyTaskExecutor.Execute(timerTask, true)
	s.Nil(err)
}

func (s *timerStandbyTaskExecutorSuite) TestProcessRetryTimeout() {

	workflowExecution := types.WorkflowExecution{
		WorkflowID: "some random workflow ID",
		RunID:      uuid.New(),
	}
	workflowType := "some random workflow type"
	taskListName := "some random task list"

	mutableState := execution.NewMutableStateBuilderWithVersionHistoriesWithEventV2(
		s.mockShard,
		s.logger,
		s.version,
		workflowExecution.GetRunID(),
		constants.TestGlobalDomainEntry,
	)
	_, err := mutableState.AddWorkflowExecutionStartedEvent(
		workflowExecution,
		&types.HistoryStartWorkflowExecutionRequest{
			DomainUUID: s.domainID,
			StartRequest: &types.StartWorkflowExecutionRequest{
				WorkflowType:                        &types.WorkflowType{Name: workflowType},
				TaskList:                            &types.TaskList{Name: taskListName},
				ExecutionStartToCloseTimeoutSeconds: common.Int32Ptr(2),
				TaskStartToCloseTimeoutSeconds:      common.Int32Ptr(1),
			},
		},
	)
	s.Nil(err)

	timerTask := s.newTimerTaskFromInfo(&persistence.TimerTaskInfo{
		Version:             s.version,
		DomainID:            s.domainID,
		WorkflowID:          workflowExecution.GetWorkflowID(),
		RunID:               workflowExecution.GetRunID(),
		TaskID:              int64(100),
		TaskType:            persistence.TaskTypeActivityRetryTimer,
		TimeoutType:         int(types.TimeoutTypeStartToClose),
		VisibilityTimestamp: s.now,
	})

	s.mockShard.SetCurrentTime(s.clusterName, s.now)
	err = s.timerStandbyTaskExecutor.Execute(timerTask, true)
	s.Nil(err)
}

func (s *timerStandbyTaskExecutorSuite) createPersistenceMutableState(
	ms execution.MutableState,
	lastEventID int64,
	lastEventVersion int64,
) *persistence.WorkflowMutableState {

	if ms.GetVersionHistories() != nil {
		currentVersionHistory, err := ms.GetVersionHistories().GetCurrentVersionHistory()
		s.NoError(err)
		err = currentVersionHistory.AddOrUpdateItem(persistence.NewVersionHistoryItem(
			lastEventID, lastEventVersion,
		))
		s.NoError(err)
	}

	return execution.CreatePersistenceMutableState(ms)
}

func (s *timerStandbyTaskExecutorSuite) newTimerTaskFromInfo(
	info *persistence.TimerTaskInfo,
) Task {
	return NewTimerTask(s.mockShard, info, QueueTypeStandbyTimer, s.logger, nil, nil, nil, nil, nil)
}
