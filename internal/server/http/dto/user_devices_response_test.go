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

func userDeviceListDataFixture() *domain.UserDeviceList {
	createdAt := time.Date(2026, 11, 13, 8, 0, 0, 123456789, time.FixedZone("UTC+8", 8*60*60))
	current := domain.UserDeviceListItem{
		UserDevice: domain.UserDevice{ID: 13, UserID: 7, Name: "学习设备 📁", Platform: "WEB", CreatedAt: createdAt},
		Current:    true,
	}
	newer := current
	newer.ID, newer.Current = ^uint64(0), false
	return &domain.UserDeviceList{Devices: []domain.UserDeviceListItem{newer, current}, HasMore: true}
}

func TestNewUserDeviceListDataJSON(t *testing.T) {
	result := userDeviceListDataFixture()
	data, err := NewUserDeviceListData(result)
	if err != nil || data == nil || len(data.Items) != 2 || !data.HasMore {
		t.Fatal("设备列表转换失败", err)
	}
	response := UserDeviceListResponse{Data: *data, RequestID: "28ee758a-0727-42de-ae7d-3bb33155e148"}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal("设备列表响应编码失败", err)
	}
	var body map[string]any
	if json.Unmarshal(encoded, &body) != nil || len(body) != 2 || body["request_id"] != response.RequestID {
		t.Fatal("外层响应字段或请求 ID 错误")
	}
	payload, valid := body["data"].(map[string]any)
	if !valid || len(payload) != 2 || payload["has_more"] != true {
		t.Fatal("列表数据或分页字段错误")
	}
	items, valid := payload["items"].([]any)
	if !valid || len(items) != 2 {
		t.Fatal("设备项数量错误")
	}
	for index, value := range items {
		item, valid := value.(map[string]any)
		if !valid || len(item) != 7 || item["id"] != strconv.FormatUint(result.Devices[index].ID, 10) ||
			item["name"] != "学习设备 📁" || item["platform"] != "WEB" || item["current"] != result.Devices[index].Current {
			t.Fatal("设备字段、字符串 ID 或布尔标记错误")
		}
		if item["created_at"] != "2026-11-13T00:00:00.123456Z" {
			t.Fatal("设备创建时间的 UTC 或微秒格式错误")
		}
		for _, name := range []string{"last_seen_at", "revoked_at"} {
			if value, exists := item[name]; !exists || value != nil {
				t.Fatal("可空时间未明确编码为 null")
			}
		}
	}
}

func TestNewUserDeviceListDataEmptyAndLimits(t *testing.T) {
	for _, result := range []*domain.UserDeviceList{{}, {Devices: []domain.UserDeviceListItem{}}} {
		data, err := NewUserDeviceListData(result)
		if err != nil || data == nil || data.Items == nil {
			t.Fatal("空列表转换失败", err)
		}
		encoded, err := json.Marshal(data)
		if err != nil || string(encoded) != "{\"items\":[],\"has_more\":false}" {
			t.Fatal("空数组或 false 分页提示被省略", err)
		}
	}
	result := userDeviceListDataFixture()
	base := result.Devices[0]
	result.Devices = make([]domain.UserDeviceListItem, 100)
	for index := range result.Devices {
		result.Devices[index] = base
		result.Devices[index].ID = uint64(100 - index)
	}
	data, err := NewUserDeviceListData(result)
	if err != nil || data == nil || len(data.Items) != 100 || !data.HasMore || data.Items[99].ID != "1" {
		t.Fatal("最大页大小转换失败", err)
	}
}

func TestNewUserDeviceListDataTimeBoundaries(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		value time.Time
	}{
		{name: "lower database year", value: time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)},
		{name: "upper database year", value: time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)},
		{name: "local year normalizes into range", value: time.Date(10000, 1, 1, 0, 30, 0, 0, time.FixedZone("UTC+1", 60*60))},
		{name: "historical time", value: time.Date(2020, 1, 1, 0, 0, 0, 123456789, time.FixedZone("UTC-5", -5*60*60))},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result := userDeviceListDataFixture()
			device := &result.Devices[0]
			device.CreatedAt, device.LastSeenAt, device.RevokedAt = testCase.value, &testCase.value, &testCase.value
			data, err := NewUserDeviceListData(result)
			if err != nil || data == nil {
				t.Fatal("合法设备时间被拒绝", err)
			}
			item := data.Items[0]
			for _, value := range []time.Time{item.CreatedAt, *item.LastSeenAt, *item.RevokedAt} {
				if !value.Equal(testCase.value.UTC().Truncate(time.Microsecond)) || value.Location() != time.UTC || value.Nanosecond()%1000 != 0 {
					t.Fatal("设备时间事实、时区或精度错误")
				}
			}
			if _, err := json.Marshal(data); err != nil {
				t.Fatal("规范化后的时间编码失败", err)
			}
		})
	}
}

func TestNewUserDeviceListDataFailures(t *testing.T) {
	if data, err := NewUserDeviceListData(nil); data != nil || !errors.Is(err, domain.ErrInvalidDeviceData) {
		t.Fatal("空服务结果未拒绝", err)
	}
	for _, testCase := range []struct {
		name   string
		change func(*domain.UserDeviceList)
	}{
		{name: "zero device id", change: func(result *domain.UserDeviceList) { result.Devices[1].ID = 0 }},
		{name: "zero owner", change: func(result *domain.UserDeviceList) { result.Devices[1].UserID = 0 }},
		{name: "too many items", change: func(result *domain.UserDeviceList) { result.Devices = make([]domain.UserDeviceListItem, 101) }},
		{name: "empty page with more", change: func(result *domain.UserDeviceList) { result.Devices = nil }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result := userDeviceListDataFixture()
			testCase.change(result)
			if data, err := NewUserDeviceListData(result); data != nil || !errors.Is(err, domain.ErrInvalidDeviceData) {
				t.Fatal("异常服务结果返回了部分 DTO 或错误分类异常", err)
			}
		})
	}
	fields := []struct {
		name string
		set  func(*domain.UserDeviceListItem, time.Time)
	}{
		{name: "created_at", set: func(item *domain.UserDeviceListItem, value time.Time) { item.CreatedAt = value }},
		{name: "last_seen_at", set: func(item *domain.UserDeviceListItem, value time.Time) { item.LastSeenAt = &value }},
		{name: "revoked_at", set: func(item *domain.UserDeviceListItem, value time.Time) { item.RevokedAt = &value }},
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
				result := userDeviceListDataFixture()
				field.set(&result.Devices[1], invalid.value)
				data, err := NewUserDeviceListData(result)
				if data != nil || !errors.Is(err, domain.ErrInvalidDeviceData) || errors.Is(err, domain.ErrInvalidSessionData) {
					t.Fatal("异常时间返回部分 DTO 或错误分类不正确", err)
				}
			})
		}
	}
}

func TestNewUserDeviceListDataCopies(t *testing.T) {
	result := userDeviceListDataFixture()
	seenAt := result.Devices[0].CreatedAt.Add(time.Minute)
	originalSeenAt := seenAt
	result.Devices[0].LastSeenAt, result.Devices[0].RevokedAt = &seenAt, &seenAt
	before := result.Devices[0]
	data, err := NewUserDeviceListData(result)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Devices[0], before) || data.Items[0].LastSeenAt == result.Devices[0].LastSeenAt ||
		data.Items[0].RevokedAt == result.Devices[0].RevokedAt || data.Items[0].LastSeenAt == data.Items[0].RevokedAt ||
		*result.Devices[0].LastSeenAt != originalSeenAt || *result.Devices[0].RevokedAt != originalSeenAt {
		t.Fatal("转换修改了输入或共享可空时间")
	}
	second, err := NewUserDeviceListData(result)
	if err != nil {
		t.Fatal(err)
	}
	*data.Items[0].LastSeenAt = seenAt.Add(time.Hour)
	*data.Items[0].RevokedAt = seenAt.Add(2 * time.Hour)
	data.Items[0].Name, data.Items[1].Current, data.HasMore = "修改副本", false, false
	if !reflect.DeepEqual(result.Devices[0], before) || second.Items[0].Name != before.Name || !second.Items[1].Current || !second.HasMore ||
		!second.Items[0].LastSeenAt.Equal(seenAt.UTC().Truncate(time.Microsecond)) || !second.Items[0].RevokedAt.Equal(seenAt.UTC().Truncate(time.Microsecond)) {
		t.Fatal("DTO 修改影响了来源或另一次转换结果")
	}
}

func TestNewUserDeviceListDataServiceResult(t *testing.T) {
	fixture := userDeviceListDataFixture()
	page := &domain.UserDevicePage{Devices: []domain.UserDevice{fixture.Devices[0].UserDevice, fixture.Devices[1].UserDevice}, HasMore: true}
	ctx := context.Background()
	devices := service.NewUserDeviceService(func(received context.Context, userID, beforeID uint64, pageSize int) (*domain.UserDevicePage, error) {
		if received != ctx || userID != 7 || beforeID != 0 || pageSize != 2 {
			t.Error("服务查询传参错误")
		}
		return page, nil
	})
	result, err := devices.List(ctx, domain.Actor{UserID: 7, SessionID: 11, DeviceID: 13}, 0, 2)
	if err != nil {
		t.Fatal("真实设备列表服务组合失败", err)
	}
	data, err := NewUserDeviceListData(result)
	if err != nil || data == nil || len(data.Items) != 2 || data.Items[0].Current || !data.Items[1].Current || !data.HasMore {
		t.Fatal("实际服务结果转换失败", err)
	}
	if _, err := json.Marshal(UserDeviceListResponse{Data: *data, RequestID: "28ee758a-0727-42de-ae7d-3bb33155e148"}); err != nil {
		t.Fatal("设备服务结果的响应编码失败", err)
	}
}
