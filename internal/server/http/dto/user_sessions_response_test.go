package dto

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func userSessionListDataFixture() *domain.UserSessionList {
	createdAt := time.Date(2026, 11, 13, 8, 0, 0, 123456789, time.FixedZone("UTC+8", 8*60*60))
	current := domain.UserSessionListItem{
		UserSession: domain.UserSession{ID: 11, UserID: 7, DeviceID: 13, DeviceName: "学习设备 📁", DevicePlatform: "WEB", CreatedAt: createdAt, ExpiresAt: createdAt.Add(time.Hour)},
		Current:     true,
	}
	newer := current
	newer.ID, newer.DeviceID, newer.Current = ^uint64(0), ^uint64(0), false
	return &domain.UserSessionList{Sessions: []domain.UserSessionListItem{newer, current}, HasMore: true}
}

func TestNewUserSessionListDataJSON(t *testing.T) {
	result := userSessionListDataFixture()
	data, err := NewUserSessionListData(result)
	if err != nil || data == nil || len(data.Items) != 2 || !data.HasMore {
		t.Fatal("会话列表转换失败", err)
	}
	response := UserSessionListResponse{Data: *data, RequestID: "28ee758a-0727-42de-ae7d-3bb33155e148"}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal("列表响应 JSON 编码失败", err)
	}
	var body map[string]any
	if err := json.Unmarshal(encoded, &body); err != nil || len(body) != 2 || body["request_id"] != response.RequestID {
		t.Fatal("外层响应字段错误", err)
	}
	payload, valid := body["data"].(map[string]any)
	if !valid || len(payload) != 2 || payload["has_more"] != true {
		t.Fatal("列表及分页字段错误")
	}
	items, valid := payload["items"].([]any)
	if !valid || len(items) != 2 {
		t.Fatal("列表项数量错误")
	}
	for index, value := range items {
		item, valid := value.(map[string]any)
		if !valid || len(item) != 8 || item["id"] != strconv.FormatUint(result.Sessions[index].ID, 10) || item["current"] != result.Sessions[index].Current {
			t.Fatal("会话标识、Current 或字段数量错误")
		}
		device, valid := item["device"].(map[string]any)
		if !valid || len(device) != 3 || device["id"] != strconv.FormatUint(result.Sessions[index].DeviceID, 10) || device["name"] != "学习设备 📁" || device["platform"] != "WEB" {
			t.Fatal("设备摘要字段或字符串标识错误")
		}
		if item["created_at"] != "2026-11-13T00:00:00.123456Z" || item["expires_at"] != "2026-11-13T01:00:00.123456Z" {
			t.Fatal("时间的 UTC 或微秒 JSON 格式错误")
		}
		for _, name := range []string{"last_seen_at", "revoked_at", "device_revoked_at"} {
			if value, exists := item[name]; !exists || value != nil {
				t.Fatal("可空时间未明确返回 null")
			}
		}
	}
}

func TestNewUserSessionListDataEmptyAndLimits(t *testing.T) {
	for _, input := range []*domain.UserSessionList{{}, {Sessions: []domain.UserSessionListItem{}}} {
		data, err := NewUserSessionListData(input)
		if err != nil || data == nil || data.Items == nil {
			t.Fatal("空列表转换失败", err)
		}
		encoded, err := json.Marshal(data)
		if err != nil || string(encoded) != `{"items":[],"has_more":false}` {
			t.Fatal("空列表或 false 分页提示被省略", err)
		}
	}
	result := userSessionListDataFixture()
	base := result.Sessions[0]
	result.Sessions = make([]domain.UserSessionListItem, 100)
	for index := range result.Sessions {
		result.Sessions[index] = base
		result.Sessions[index].ID = uint64(100 - index)
	}
	data, err := NewUserSessionListData(result)
	if err != nil || data == nil || len(data.Items) != 100 || !data.HasMore || data.Items[99].ID != "1" {
		t.Fatal("最大页大小转换失败", err)
	}
}

func TestNewUserSessionListDataTimeBoundaries(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		value time.Time
	}{
		{name: "database lower year", value: time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)},
		{name: "database upper year", value: time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)},
		{name: "local year normalizes into utc range", value: time.Date(10000, 1, 1, 0, 30, 0, 0, time.FixedZone("UTC+1", 60*60))},
		{name: "historical time", value: time.Date(2020, 1, 1, 0, 0, 0, 123456789, time.FixedZone("UTC-5", -5*60*60))},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result := userSessionListDataFixture()
			session := &result.Sessions[0]
			session.CreatedAt, session.ExpiresAt = testCase.value, testCase.value
			session.LastSeenAt, session.RevokedAt, session.DeviceRevokedAt = &testCase.value, &testCase.value, &testCase.value
			data, err := NewUserSessionListData(result)
			if err != nil || data == nil {
				t.Fatal("合法时间被拒绝", err)
			}
			item := data.Items[0]
			for _, value := range []time.Time{item.CreatedAt, item.ExpiresAt, *item.LastSeenAt, *item.RevokedAt, *item.DeviceRevokedAt} {
				if !value.Equal(testCase.value.UTC().Truncate(time.Microsecond)) || value.Location() != time.UTC || value.Nanosecond()%1000 != 0 {
					t.Fatal("时间事实、时区或精度不正确")
				}
			}
			if _, err := json.Marshal(data); err != nil {
				t.Fatal("规范化后的合法时间无法编码", err)
			}
		})
	}
}

func TestNewUserSessionListDataFailures(t *testing.T) {
	if data, err := NewUserSessionListData(nil); data != nil || !errors.Is(err, domain.ErrInvalidSessionData) {
		t.Fatal("空服务结果未拒绝", err)
	}
	for _, testCase := range []struct {
		name   string
		change func(*domain.UserSessionList)
	}{
		{name: "zero session", change: func(result *domain.UserSessionList) { result.Sessions[1].ID = 0 }},
		{name: "zero owner", change: func(result *domain.UserSessionList) { result.Sessions[1].UserID = 0 }},
		{name: "zero device", change: func(result *domain.UserSessionList) { result.Sessions[1].DeviceID = 0 }},
		{name: "too many items", change: func(result *domain.UserSessionList) { result.Sessions = make([]domain.UserSessionListItem, 101) }},
		{name: "empty page with more", change: func(result *domain.UserSessionList) { result.Sessions = nil }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result := userSessionListDataFixture()
			testCase.change(result)
			if data, err := NewUserSessionListData(result); data != nil || !errors.Is(err, domain.ErrInvalidSessionData) {
				t.Fatal("异常服务结果未拒绝或返回了部分 DTO", err)
			}
		})
	}
	fields := []struct {
		name string
		set  func(*domain.UserSessionListItem, time.Time)
	}{
		{name: "created_at", set: func(item *domain.UserSessionListItem, value time.Time) { item.CreatedAt = value }},
		{name: "expires_at", set: func(item *domain.UserSessionListItem, value time.Time) { item.ExpiresAt = value }},
		{name: "last_seen_at", set: func(item *domain.UserSessionListItem, value time.Time) { item.LastSeenAt = &value }},
		{name: "revoked_at", set: func(item *domain.UserSessionListItem, value time.Time) { item.RevokedAt = &value }},
		{name: "device_revoked_at", set: func(item *domain.UserSessionListItem, value time.Time) { item.DeviceRevokedAt = &value }},
	}
	for _, field := range fields {
		for _, invalid := range []struct {
			name  string
			value time.Time
		}{
			{name: "zero time"},
			{name: "year too small", value: time.Date(999, 1, 1, 0, 0, 0, 0, time.UTC)},
			{name: "year too large", value: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)},
			{name: "utc year below range", value: time.Date(1000, 1, 1, 0, 30, 0, 0, time.FixedZone("UTC+1", 60*60))},
			{name: "utc year above range", value: time.Date(9999, 12, 31, 23, 30, 0, 0, time.FixedZone("UTC-1", -60*60))},
		} {
			t.Run(field.name+"/"+invalid.name, func(t *testing.T) {
				result := userSessionListDataFixture()
				field.set(&result.Sessions[1], invalid.value)
				if data, err := NewUserSessionListData(result); data != nil || !errors.Is(err, domain.ErrInvalidSessionData) {
					t.Fatal("异常时间未拒绝或返回了部分 DTO", err)
				}
			})
		}
	}
}

func TestNewUserSessionListDataCopies(t *testing.T) {
	result := userSessionListDataFixture()
	seenAt := result.Sessions[0].CreatedAt.Add(time.Minute)
	originalSeenAt := seenAt
	result.Sessions[0].LastSeenAt, result.Sessions[0].RevokedAt, result.Sessions[0].DeviceRevokedAt = &seenAt, &seenAt, &seenAt
	before := result.Sessions[0]
	data, err := NewUserSessionListData(result)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Sessions[0], before) || data.Items[0].LastSeenAt == result.Sessions[0].LastSeenAt ||
		data.Items[0].RevokedAt == result.Sessions[0].RevokedAt || data.Items[0].DeviceRevokedAt == result.Sessions[0].DeviceRevokedAt || data.Items[0].LastSeenAt == data.Items[0].RevokedAt ||
		*result.Sessions[0].LastSeenAt != originalSeenAt || *result.Sessions[0].RevokedAt != originalSeenAt || *result.Sessions[0].DeviceRevokedAt != originalSeenAt {
		t.Fatal("转换修改了输入或共享可空时间")
	}
	second, err := NewUserSessionListData(result)
	if err != nil {
		t.Fatal(err)
	}
	*data.Items[0].LastSeenAt = seenAt.Add(time.Hour)
	*data.Items[0].RevokedAt = seenAt.Add(2 * time.Hour)
	*data.Items[0].DeviceRevokedAt = seenAt.Add(3 * time.Hour)
	data.Items[0].Device.Name, data.Items[1].Current, data.HasMore = "修改副本", false, false
	if !reflect.DeepEqual(result.Sessions[0], before) || second.Items[0].Device.Name != before.DeviceName || !second.Items[1].Current || !second.HasMore ||
		!second.Items[0].LastSeenAt.Equal(seenAt.UTC().Truncate(time.Microsecond)) || !second.Items[0].RevokedAt.Equal(seenAt.UTC().Truncate(time.Microsecond)) || !second.Items[0].DeviceRevokedAt.Equal(seenAt.UTC().Truncate(time.Microsecond)) {
		t.Fatal("修改 DTO 影响了来源或另一次转换结果")
	}
}

func TestNewUserSessionListDataServiceResult(t *testing.T) {
	fixture := userSessionListDataFixture()
	page := &domain.UserSessionPage{Sessions: []domain.UserSession{fixture.Sessions[0].UserSession, fixture.Sessions[1].UserSession}, HasMore: true}
	ctx := context.Background()
	sessions := service.NewUserSessionService(func(received context.Context, userID, beforeID uint64, pageSize int) (*domain.UserSessionPage, error) {
		if received != ctx || userID != 7 || beforeID != 0 || pageSize != 2 {
			t.Error("服务查询传参错误")
		}
		return page, nil
	})
	result, err := sessions.List(ctx, domain.Actor{UserID: 7, SessionID: 11, DeviceID: 13}, 0, 2)
	if err != nil {
		t.Fatal("真实列表服务组合失败", err)
	}
	data, err := NewUserSessionListData(result)
	if err != nil || data == nil || len(data.Items) != 2 || data.Items[0].Current || !data.Items[1].Current || !data.HasMore {
		t.Fatal("实际服务结果转换失败", err)
	}
	if _, err := json.Marshal(UserSessionListResponse{Data: *data, RequestID: "28ee758a-0727-42de-ae7d-3bb33155e148"}); err != nil {
		t.Fatal("服务结果的响应编码失败", err)
	}
}
