// Copyright (c) 2017 Uber Technologies, Inc.
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

package configstore

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/config"
	dc "github.com/uber/cadence/common/dynamicconfig"
	"github.com/uber/cadence/common/log"
	p "github.com/uber/cadence/common/persistence"
	"github.com/uber/cadence/common/persistence/nosql"
	"github.com/uber/cadence/common/persistence/nosql/nosqlplugin"
	"github.com/uber/cadence/common/types"
)

const (
	retryAttempts = 2
)

type eqSnapshotVersionMatcher struct {
	version int64
}

func (e eqSnapshotVersionMatcher) Matches(x interface{}) bool {
	arg, ok := x.(*p.UpdateDynamicConfigRequest)
	if !ok {
		return false
	}

	return e.version == arg.Snapshot.Version
}

func (e eqSnapshotVersionMatcher) String() string {
	return fmt.Sprintf("Version match %d.\n", e.version)
}

func EqSnapshotVersion(version int64) gomock.Matcher {
	return eqSnapshotVersionMatcher{version}
}

type configStoreClientSuite struct {
	suite.Suite
	*require.Assertions
	client         dc.Client
	mockManager    *p.MockConfigStoreManager
	mockController *gomock.Controller
	doneCh         chan struct{}
}

var snapshot1 *p.DynamicConfigSnapshot

func TestConfigStoreClientSuite(t *testing.T) {
	snapshot1 = &p.DynamicConfigSnapshot{
		Version: 1,
		Values: &types.DynamicConfigBlob{
			SchemaVersion: 1,
			Entries: []*types.DynamicConfigEntry{
				{
					Name:         dc.Keys[dc.TestGetBoolPropertyKey],
					DefaultValue: nil,
					Values: []*types.DynamicConfigValue{
						{
							Value: &types.DataBlob{
								EncodingType: types.EncodingTypeJSON.Ptr(),
								Data:         jsonMarshalHelper(false),
							},
							Filters: nil,
						},
						{
							Value: &types.DataBlob{
								EncodingType: types.EncodingTypeJSON.Ptr(),
								Data:         jsonMarshalHelper(true),
							},
							Filters: []*types.DynamicConfigFilter{
								{
									Name: "domainName",
									Value: &types.DataBlob{
										EncodingType: types.EncodingTypeJSON.Ptr(),
										Data:         jsonMarshalHelper("global-samples-domain"),
									},
								},
							},
						},
						{
							Value: &types.DataBlob{
								EncodingType: types.EncodingTypeJSON.Ptr(),
								Data:         jsonMarshalHelper(true),
							},
							Filters: []*types.DynamicConfigFilter{
								{
									Name: "domainName",
									Value: &types.DataBlob{
										EncodingType: types.EncodingTypeJSON.Ptr(),
										Data:         jsonMarshalHelper("samples-domain"),
									},
								},
							},
						},
					},
				},
				{
					Name:         dc.Keys[dc.TestGetIntPropertyKey],
					DefaultValue: nil,
					Values: []*types.DynamicConfigValue{
						{
							Value: &types.DataBlob{
								EncodingType: types.EncodingTypeJSON.Ptr(),
								Data:         jsonMarshalHelper(1000),
							},
							Filters: nil,
						},
						{
							Value: &types.DataBlob{
								EncodingType: types.EncodingTypeJSON.Ptr(),
								Data:         jsonMarshalHelper(1000.1),
							},
							Filters: []*types.DynamicConfigFilter{
								{
									Name: "domainName",
									Value: &types.DataBlob{
										EncodingType: types.EncodingTypeJSON.Ptr(),
										Data:         jsonMarshalHelper("global-samples-domain"),
									},
								},
							},
						},
					},
				},
				{
					Name:         dc.Keys[dc.TestGetFloat64PropertyKey],
					DefaultValue: nil,
					Values: []*types.DynamicConfigValue{
						{
							Value: &types.DataBlob{
								EncodingType: types.EncodingTypeJSON.Ptr(),
								Data:         jsonMarshalHelper(12),
							},
							Filters: nil,
						},
						{
							Value: &types.DataBlob{
								EncodingType: types.EncodingTypeJSON.Ptr(),
								Data:         jsonMarshalHelper("wrong type"),
							},
							Filters: []*types.DynamicConfigFilter{
								{
									Name: "domainName",
									Value: &types.DataBlob{
										EncodingType: types.EncodingTypeJSON.Ptr(),
										Data:         jsonMarshalHelper("samples-domain"),
									},
								},
							},
						},
					},
				},
				{
					Name:         dc.Keys[dc.TestGetStringPropertyKey],
					DefaultValue: nil,
					Values: []*types.DynamicConfigValue{
						{
							Value: &types.DataBlob{
								EncodingType: types.EncodingTypeJSON.Ptr(),
								Data:         jsonMarshalHelper("some random string"),
							},
							Filters: nil,
						},
						{
							Value: &types.DataBlob{
								EncodingType: types.EncodingTypeJSON.Ptr(),
								Data:         jsonMarshalHelper("constrained-string"),
							},
							Filters: []*types.DynamicConfigFilter{
								{
									Name: "taskListName",
									Value: &types.DataBlob{
										EncodingType: types.EncodingTypeJSON.Ptr(),
										Data:         jsonMarshalHelper("random tasklist"),
									},
								},
							},
						},
					},
				},
				{
					Name:         dc.Keys[dc.TestGetMapPropertyKey],
					DefaultValue: nil,
					Values: []*types.DynamicConfigValue{
						{
							Value: &types.DataBlob{
								EncodingType: types.EncodingTypeJSON.Ptr(),
								Data: jsonMarshalHelper(map[string]interface{}{
									"key1": "1",
									"key2": 1,
									"key3": []interface{}{
										false,
										map[string]interface{}{
											"key4": true,
											"key5": 2.1,
										},
									},
								}),
							},
							Filters: nil,
						},
						{
							Value: &types.DataBlob{
								EncodingType: types.EncodingTypeJSON.Ptr(),
								Data:         jsonMarshalHelper("1"),
							},
							Filters: []*types.DynamicConfigFilter{
								{
									Name: "taskListName",
									Value: &types.DataBlob{
										EncodingType: types.EncodingTypeJSON.Ptr(),
										Data:         jsonMarshalHelper("random tasklist"),
									},
								},
							},
						},
					},
				},
				{
					Name:         dc.Keys[dc.TestGetDurationPropertyKey],
					DefaultValue: nil,
					Values: []*types.DynamicConfigValue{
						{
							Value: &types.DataBlob{
								EncodingType: types.EncodingTypeJSON.Ptr(),
								Data:         jsonMarshalHelper("1m"),
							},
							Filters: nil,
						},
						{
							Value: &types.DataBlob{
								EncodingType: types.EncodingTypeJSON.Ptr(),
								Data:         jsonMarshalHelper("wrong duration string"),
							},
							Filters: []*types.DynamicConfigFilter{
								{
									Name: "domainName",
									Value: &types.DataBlob{
										EncodingType: types.EncodingTypeJSON.Ptr(),
										Data:         jsonMarshalHelper("samples-domain"),
									},
								},
								{
									Name: "taskListName",
									Value: &types.DataBlob{
										EncodingType: types.EncodingTypeJSON.Ptr(),
										Data:         jsonMarshalHelper("longIdleTimeTaskList"),
									},
								},
							},
						},
						{
							Value: &types.DataBlob{
								EncodingType: types.EncodingTypeJSON.Ptr(),
								Data:         jsonMarshalHelper(2),
							},
							Filters: []*types.DynamicConfigFilter{
								{
									Name: "domainName",
									Value: &types.DataBlob{
										EncodingType: types.EncodingTypeJSON.Ptr(),
										Data:         jsonMarshalHelper("samples-domain"),
									},
								},
							},
						},
					},
				},
			},
		},
	}
	s := new(configStoreClientSuite)
	suite.Run(t, s)
}

func (s *configStoreClientSuite) SetupSuite() {
	s.doneCh = make(chan struct{})
	s.mockController = gomock.NewController(s.T())

	mockDB := nosqlplugin.NewMockDB(s.mockController)
	mockDB.EXPECT().
		InsertConfig(gomock.Any(), gomock.Any()).
		Return(nil).AnyTimes()

	blob, err := p.NewPayloadSerializer().SerializeDynamicConfigBlob(snapshot1.Values, common.EncodingTypeThriftRW)
	s.Require().NoError(err)

	mockDB.EXPECT().
		SelectLatestConfig(gomock.Any(), gomock.Any()).
		Return(&p.InternalConfigStoreEntry{
			RowType:   1,
			Version:   1,
			Timestamp: time.Now(),
			Values:    blob,
		}, nil).AnyTimes()

	mockDB.EXPECT().
		IsNotFoundError(gomock.Any()).
		Return(true).AnyTimes()

	mockPlugin := nosqlplugin.NewMockPlugin(s.mockController)
	mockPlugin.EXPECT().
		CreateDB(gomock.Any(), gomock.Any()).
		Return(mockDB, nil).AnyTimes()
	nosql.RegisterPlugin("cassandra", mockPlugin)

	s.client, err = NewConfigStoreClient(
		&ConfigStoreClientConfig{
			PollInterval:        time.Second * 2,
			UpdateRetryAttempts: retryAttempts,
			FetchTimeout:        time.Second * 1,
			UpdateTimeout:       time.Second * 1,
		},
		&config.NoSQL{
			PluginName: "cassandra",
		}, log.NewNoop(), s.doneCh)
	s.Require().NoError(err)

	s.mockManager = p.NewMockConfigStoreManager(s.mockController)
	configStoreClient, ok := s.client.(*configStoreClient)
	s.Require().True(ok)
	configStoreClient.configStoreManager = s.mockManager

	s.mockManager.EXPECT().
		FetchDynamicConfig(gomock.Any()).
		Return(&p.FetchDynamicConfigResponse{
			Snapshot: snapshot1,
		}, nil).
		AnyTimes()

	time.Sleep(2 * time.Second)
}

func (s *configStoreClientSuite) TearDownSuite() {
	close(s.doneCh)
}

func (s *configStoreClientSuite) SetupTest() {
	s.Assertions = require.New(s.T())

	s.mockManager = p.NewMockConfigStoreManager(s.mockController)
	configStoreClient, ok := s.client.(*configStoreClient)
	s.Require().True(ok)
	configStoreClient.configStoreManager = s.mockManager

	s.mockManager.EXPECT().
		FetchDynamicConfig(gomock.Any()).
		Return(&p.FetchDynamicConfigResponse{
			Snapshot: snapshot1,
		}, nil).
		AnyTimes()
}

func (s *configStoreClientSuite) TestGetValue() {
	v, err := s.client.GetValue(dc.TestGetBoolPropertyKey, true)
	s.NoError(err)
	s.Equal(false, v)
}

func (s *configStoreClientSuite) TestGetValue_NonExistKey() {
	v, err := s.client.GetValue(dc.LastKeyForTest, true)
	s.Error(err)
	s.Equal(v, true)
}

func (s *configStoreClientSuite) TestGetValueWithFilters() {
	filters := map[dc.Filter]interface{}{
		dc.DomainName: "global-samples-domain",
	}
	v, err := s.client.GetValueWithFilters(dc.TestGetBoolPropertyKey, filters, false)
	s.NoError(err)
	s.Equal(true, v)

	filters = map[dc.Filter]interface{}{
		dc.DomainName: "non-exist-domain",
	}
	v, err = s.client.GetValueWithFilters(dc.TestGetBoolPropertyKey, filters, true)
	s.NoError(err)
	s.Equal(false, v)

	filters = map[dc.Filter]interface{}{
		dc.DomainName:   "samples-domain",
		dc.TaskListName: "non-exist-tasklist",
	}
	v, err = s.client.GetValueWithFilters(dc.TestGetBoolPropertyKey, filters, false)
	s.NoError(err)
	s.Equal(true, v)
}

func (s *configStoreClientSuite) TestGetValueWithFilters_UnknownFilter() {
	filters := map[dc.Filter]interface{}{
		dc.DomainName:    "global-samples-domain1",
		dc.UnknownFilter: "unknown-filter1",
	}
	v, err := s.client.GetValueWithFilters(dc.TestGetBoolPropertyKey, filters, false)
	s.NoError(err)
	s.Equal(false, v)
}

func (s *configStoreClientSuite) TestGetIntValue() {
	v, err := s.client.GetIntValue(dc.TestGetIntPropertyKey, nil, 1)
	s.NoError(err)
	s.Equal(1000, v)
}

func (s *configStoreClientSuite) TestGetIntValue_FilterNotMatch() {
	filters := map[dc.Filter]interface{}{
		dc.DomainName: "samples-domain",
	}
	v, err := s.client.GetIntValue(dc.TestGetIntPropertyKey, filters, 500)
	s.NoError(err)
	s.Equal(1000, v)
}

func (s *configStoreClientSuite) TestGetIntValue_WrongType() {
	defaultValue := 2000
	filters := map[dc.Filter]interface{}{
		dc.DomainName: "global-samples-domain",
	}
	v, err := s.client.GetIntValue(dc.TestGetIntPropertyKey, filters, defaultValue)
	s.Error(err)
	s.Equal(defaultValue, v)
}

func (s *configStoreClientSuite) TestGetFloatValue() {
	v, err := s.client.GetFloatValue(dc.TestGetFloat64PropertyKey, nil, 1)
	s.NoError(err)
	s.Equal(12.0, v)
}

func (s *configStoreClientSuite) TestGetFloatValue_WrongType() {
	filters := map[dc.Filter]interface{}{
		dc.DomainName: "samples-domain",
	}
	defaultValue := 1.0
	v, err := s.client.GetFloatValue(dc.TestGetFloat64PropertyKey, filters, defaultValue)
	s.Error(err)
	s.Equal(defaultValue, v)
}

func (s *configStoreClientSuite) TestGetBoolValue() {
	v, err := s.client.GetBoolValue(dc.TestGetBoolPropertyKey, nil, true)
	s.NoError(err)
	s.Equal(false, v)
}

func (s *configStoreClientSuite) TestGetStringValue() {
	filters := map[dc.Filter]interface{}{
		dc.TaskListName: "random tasklist",
	}
	v, err := s.client.GetStringValue(dc.TestGetStringPropertyKey, filters, "defaultString")
	s.NoError(err)
	s.Equal("constrained-string", v)
}

//not working, json float vs int on key2
func (s *configStoreClientSuite) TestGetMapValue() {
	var defaultVal map[string]interface{}
	v, err := s.client.GetMapValue(dc.TestGetMapPropertyKey, nil, defaultVal)
	s.NoError(err)
	expectedVal := map[string]interface{}{
		"key1": "1",
		"key2": 1,
		"key3": []interface{}{
			false,
			map[string]interface{}{
				"key4": true,
				"key5": 2.1,
			},
		},
	}
	s.Equal(expectedVal, v)
}

func (s *configStoreClientSuite) TestGetMapValue_WrongType() {
	var defaultVal map[string]interface{}
	filters := map[dc.Filter]interface{}{
		dc.TaskListName: "random tasklist",
	}
	v, err := s.client.GetMapValue(dc.TestGetMapPropertyKey, filters, defaultVal)
	s.Error(err)
	s.Equal(defaultVal, v)
}

func (s *configStoreClientSuite) TestGetDurationValue() {
	v, err := s.client.GetDurationValue(dc.TestGetDurationPropertyKey, nil, time.Second)
	s.NoError(err)
	s.Equal(time.Minute, v)
}

func (s *configStoreClientSuite) TestGetDurationValue_NotStringRepresentation() {
	filters := map[dc.Filter]interface{}{
		dc.DomainName: "samples-domain",
	}
	v, err := s.client.GetDurationValue(dc.TestGetDurationPropertyKey, filters, time.Second)
	s.Error(err)
	s.Equal(time.Second, v)
}

func (s *configStoreClientSuite) TestGetDurationValue_ParseFailed() {
	filters := map[dc.Filter]interface{}{
		dc.DomainName:   "samples-domain",
		dc.TaskListName: "longIdleTimeTasklist",
	}
	v, err := s.client.GetDurationValue(dc.TestGetDurationPropertyKey, filters, time.Second)
	s.Error(err)
	s.Equal(time.Second, v)
}

func (s *configStoreClientSuite) TestValidateConfig_ConfigNotExist() {
	_, err := NewConfigStoreClient(nil, nil, nil, nil)
	s.Error(err)
}

func (s *configStoreClientSuite) TestValidateConfig_InvalidConfig() {
	_, err := NewConfigStoreClient(
		&ConfigStoreClientConfig{
			PollInterval:        time.Second * 1,
			UpdateRetryAttempts: 0,
			FetchTimeout:        time.Second * 3,
			UpdateTimeout:       time.Second * 4,
		},
		nil, nil, nil)
	s.Error(err)

	_, err = NewConfigStoreClient(
		&ConfigStoreClientConfig{
			PollInterval:        time.Second * 2,
			UpdateRetryAttempts: -1,
			FetchTimeout:        time.Second * 2,
			UpdateTimeout:       time.Second * 2,
		},
		nil, nil, nil)
	s.Error(err)

	_, err = NewConfigStoreClient(
		&ConfigStoreClientConfig{
			PollInterval:        time.Second * 2,
			UpdateRetryAttempts: 0,
			FetchTimeout:        time.Second * 0,
			UpdateTimeout:       time.Second * 0,
		},
		nil, nil, nil)
	s.Error(err)
}

func (s *configStoreClientSuite) TestMatchFilters() {
	testCases := []struct {
		v       *types.DynamicConfigValue
		filters map[dc.Filter]interface{}
		matched bool
	}{
		{
			v: &types.DynamicConfigValue{
				Value:   nil,
				Filters: nil,
			},
			filters: map[dc.Filter]interface{}{
				dc.DomainName: "some random domain",
			},
			matched: true,
		},
		{
			v: &types.DynamicConfigValue{
				Value: nil,
				Filters: []*types.DynamicConfigFilter{
					{
						Name: "some key",
						Value: &types.DataBlob{
							EncodingType: types.EncodingTypeJSON.Ptr(),
							Data:         jsonMarshalHelper("some value"),
						},
					},
				},
			},
			filters: map[dc.Filter]interface{}{},
			matched: false,
		},
		{
			v: &types.DynamicConfigValue{
				Value: nil,
				Filters: []*types.DynamicConfigFilter{
					{
						Name: "domainName",
						Value: &types.DataBlob{
							EncodingType: types.EncodingTypeJSON.Ptr(),
							Data:         jsonMarshalHelper("samples-domain"),
						},
					},
				},
			},
			filters: map[dc.Filter]interface{}{
				dc.DomainName: "some random domain",
			},
			matched: false,
		},
		{
			v: &types.DynamicConfigValue{
				Value: nil,
				Filters: []*types.DynamicConfigFilter{
					{
						Name: "domainName",
						Value: &types.DataBlob{
							EncodingType: types.EncodingTypeJSON.Ptr(),
							Data:         jsonMarshalHelper("samples-domain"),
						},
					},
					{
						Name: "taskListName",
						Value: &types.DataBlob{
							EncodingType: types.EncodingTypeJSON.Ptr(),
							Data:         jsonMarshalHelper("sample-task-list"),
						},
					},
				},
			},
			filters: map[dc.Filter]interface{}{
				dc.DomainName:   "samples-domain",
				dc.TaskListName: "sample-task-list",
			},
			matched: true,
		},
		{
			v: &types.DynamicConfigValue{
				Value: nil,
				Filters: []*types.DynamicConfigFilter{
					{
						Name: "domainName",
						Value: &types.DataBlob{
							EncodingType: types.EncodingTypeJSON.Ptr(),
							Data:         jsonMarshalHelper("samples-domain"),
						},
					},
					{
						Name: "some-other-filter",
						Value: &types.DataBlob{
							EncodingType: types.EncodingTypeJSON.Ptr(),
							Data:         jsonMarshalHelper("sample-task-list"),
						},
					},
				},
			},
			filters: map[dc.Filter]interface{}{
				dc.DomainName:   "samples-domain",
				dc.TaskListName: "sample-task-list",
			},
			matched: false,
		},
		{
			v: &types.DynamicConfigValue{
				Value: nil,
				Filters: []*types.DynamicConfigFilter{
					{
						Name: "domainName",
						Value: &types.DataBlob{
							EncodingType: types.EncodingTypeJSON.Ptr(),
							Data:         jsonMarshalHelper("samples-domain"),
						},
					},
				},
			},
			filters: map[dc.Filter]interface{}{
				dc.TaskListName: "sample-task-list",
			},
			matched: false,
		},
	}

	for index, tc := range testCases {
		matched := matchFilters(tc.v, tc.filters)
		s.Equal(tc.matched, matched, fmt.Sprintf("Test case %v failed", index))
	}
}

func (s *configStoreClientSuite) TestUpdateConfig_NoRetrySuccess() {
	s.mockManager.EXPECT().
		UpdateDynamicConfig(gomock.Any(), EqSnapshotVersion(2)).
		Return(nil).MaxTimes(1)

	values := []*types.DynamicConfigValue{
		{
			Value: &types.DataBlob{
				EncodingType: types.EncodingTypeJSON.Ptr(),
				Data:         jsonMarshalHelper(true),
			},
			Filters: nil,
		},
	}
	err := s.client.UpdateValue(dc.TestGetBoolPropertyKey, values)
	s.NoError(err)

	snapshot2 := snapshot1
	snapshot2.Values.Entries[0].Values = values
	s.mockManager.EXPECT().
		FetchDynamicConfig(gomock.Any()).
		Return(&p.FetchDynamicConfigResponse{
			Snapshot: snapshot2,
		}, nil).MaxTimes(1)

	time.Sleep(2)

	v, err := s.client.GetValue(dc.TestGetBoolPropertyKey, false)
	s.NoError(err)
	s.Equal(true, v)
}

func (s *configStoreClientSuite) TestUpdateConfig_RetrySuccess() {
	s.mockManager.EXPECT().
		UpdateDynamicConfig(gomock.Any(), EqSnapshotVersion(2)).
		Return(&p.ConditionFailedError{}).AnyTimes()

	s.mockManager.EXPECT().
		UpdateDynamicConfig(gomock.Any(), EqSnapshotVersion(3)).
		Return(nil).AnyTimes()

	snapshot1.Version = 2
	s.mockManager.EXPECT().
		FetchDynamicConfig(gomock.Any()).
		Return(&p.FetchDynamicConfigResponse{
			Snapshot: snapshot1,
		}, nil).AnyTimes()

	err := s.client.UpdateValue(dc.TestGetBoolPropertyKey, []*types.DynamicConfigValue{})
	s.NoError(err)
}

func (s *configStoreClientSuite) TestUpdateConfig_RetryFailure() {
	s.mockManager.EXPECT().
		UpdateDynamicConfig(gomock.Any(), gomock.Any()).
		Return(&p.ConditionFailedError{}).MaxTimes(retryAttempts + 1)

	err := s.client.UpdateValue(dc.TestGetFloat64PropertyKey, []*types.DynamicConfigValue{})
	s.Error(err)
}

func (s *configStoreClientSuite) TestUpdateTimeout() {
	s.mockManager.EXPECT().
		UpdateDynamicConfig(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ *p.UpdateDynamicConfigRequest) error {
			time.Sleep(2 * time.Second)
			return nil
		}).AnyTimes()

	err := s.client.UpdateValue(dc.TestGetDurationPropertyKey, []*types.DynamicConfigValue{})
	s.Error(err)
}

func jsonMarshalHelper(v interface{}) []byte {
	data, _ := json.Marshal(v)
	return data
}
