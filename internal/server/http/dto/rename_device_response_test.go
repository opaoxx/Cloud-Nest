package dto

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestNewRenameDeviceDataJSON(t *testing.T) {
	// 1. 核对完整 uint64、名称原文、时间和精确 JSON 字段。
	result := userDeviceListDataFixture().Devices[0].UserDevice
	result.Name = "  Study PC 🎮  "
	data, err := NewRenameDeviceData(&result)
	if err != nil || data == nil {
		t.Fatal("设备改名结果转换失败", err)
	}
	response := RenameDeviceResponse{Data: *data, RequestID: "28ee758a-0727-42de-ae7d-3bb33155e148"}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal("设备改名响应编码失败", err)
	}
	var body map[string]any
	if json.Unmarshal(encoded, &body) != nil || len(body) != 2 || body["request_id"] != response.RequestID {
		t.Fatal("设备改名响应外层字段错误")
	}
	payload, valid := body["data"].(map[string]any)
	if !valid || len(payload) != 1 {
		t.Fatal("改名响应 data 字段错误")
	}
	device, valid := payload["device"].(map[string]any)
	expected := map[string]any{
		"id": "18446744073709551615", "name": result.Name, "platform": result.Platform,
		"created_at": "2026-11-13T00:00:00.123456Z", "last_seen_at": nil, "revoked_at": nil,
	}
	if !valid || !reflect.DeepEqual(device, expected) {
		t.Fatal("设备字段、字符串 ID、名称或时间错误")
	}
}

func TestNewRenameDeviceDataTimeBoundaries(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		value time.Time
	}{
		{name: "lower database year", value: time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)},
		{name: "upper database year", value: time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)},
		{name: "local year normalizes into range", value: time.Date(10000, 1, 1, 0, 30, 0, 0, time.FixedZone("UTC+1", 60*60))},
		{name: "historical revoked device", value: time.Date(2020, 1, 1, 0, 0, 0, 123456789, time.FixedZone("UTC-5", -5*60*60))},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			result := userDeviceListDataFixture().Devices[1].UserDevice
			result.CreatedAt, result.LastSeenAt, result.RevokedAt = testCase.value, &testCase.value, &testCase.value
			data, err := NewRenameDeviceData(&result)
			if err != nil || data == nil {
				t.Fatal("合法改名设备时间被拒绝", err)
			}
			for _, value := range []time.Time{data.Device.CreatedAt, *data.Device.LastSeenAt, *data.Device.RevokedAt} {
				if !value.Equal(testCase.value.UTC().Truncate(time.Microsecond)) || value.Location() != time.UTC || value.Nanosecond()%1000 != 0 {
					t.Fatal("改名设备时间事实、时区或精度错误")
				}
			}
			if _, err := json.Marshal(data); err != nil {
				t.Fatal("规范化后的设备时间编码失败", err)
			}
		})
	}
}

func TestNewRenameDeviceDataFailures(t *testing.T) {
	if data, err := NewRenameDeviceData(nil); data != nil || !errors.Is(err, domain.ErrInvalidDeviceData) {
		t.Fatal("空改名结果未拒绝", err)
	}
	for _, field := range []string{"id", "user_id"} {
		t.Run(field, func(t *testing.T) {
			result := userDeviceListDataFixture().Devices[1].UserDevice
			if field == "id" {
				result.ID = 0
			} else {
				result.UserID = 0
			}
			if data, err := NewRenameDeviceData(&result); data != nil || !errors.Is(err, domain.ErrInvalidDeviceData) {
				t.Fatal("无效标识未拒绝", err)
			}
		})
	}
	for _, field := range []struct {
		name string
		set  func(*domain.UserDevice, time.Time)
	}{
		{name: "created_at", set: func(device *domain.UserDevice, value time.Time) { device.CreatedAt = value }},
		{name: "last_seen_at", set: func(device *domain.UserDevice, value time.Time) { device.LastSeenAt = &value }},
		{name: "revoked_at", set: func(device *domain.UserDevice, value time.Time) { device.RevokedAt = &value }},
	} {
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
				result := userDeviceListDataFixture().Devices[1].UserDevice
				field.set(&result, invalid.value)
				data, err := NewRenameDeviceData(&result)
				if data != nil || !errors.Is(err, domain.ErrInvalidDeviceData) || errors.Is(err, domain.ErrInvalidSessionData) {
					t.Fatal("非法时间返回了部分结果或错误分类异常", err)
				}
			})
		}
	}
}

func TestNewRenameDeviceDataCopies(t *testing.T) {
	result := userDeviceListDataFixture().Devices[0].UserDevice
	seenAt := result.CreatedAt.Add(time.Minute)
	originalSeenAt := seenAt
	result.LastSeenAt, result.RevokedAt = &seenAt, &seenAt
	before := result
	data, err := NewRenameDeviceData(&result)
	if err != nil || data == nil {
		t.Fatal("改名转换失败", err)
	}
	if !reflect.DeepEqual(result, before) || *result.LastSeenAt != originalSeenAt || *result.RevokedAt != originalSeenAt ||
		data.Device.LastSeenAt == result.LastSeenAt || data.Device.RevokedAt == result.RevokedAt || data.Device.LastSeenAt == data.Device.RevokedAt {
		t.Fatal("转换修改了输入或共享可空时间")
	}
	second, err := NewRenameDeviceData(&result)
	if err != nil {
		t.Fatal(err)
	}
	data.Device.Name = "修改副本"
	*data.Device.LastSeenAt = seenAt.Add(time.Hour)
	*data.Device.RevokedAt = seenAt.Add(2 * time.Hour)
	if !reflect.DeepEqual(result, before) || *result.LastSeenAt != originalSeenAt || *result.RevokedAt != originalSeenAt || second.Device.Name != before.Name ||
		!second.Device.LastSeenAt.Equal(seenAt.UTC().Truncate(time.Microsecond)) || !second.Device.RevokedAt.Equal(seenAt.UTC().Truncate(time.Microsecond)) {
		t.Fatal("DTO 修改影响了来源或另一次转换结果")
	}
}

type renameDeviceDataTransaction struct {
	device *domain.UserDevice
}

func (transaction renameDeviceDataTransaction) RenameUserDevice(_ context.Context, targetID uint64, name string) (*domain.UserDevice, error) {
	device := *transaction.device
	device.ID, device.Name = targetID, name
	return &device, nil
}

func TestNewRenameDeviceDataServiceResult(t *testing.T) {
	// 1. 使用真实 Service 和内存事务适配器，核对提交后结果与提交失败。
	fault := errors.New("fixture commit fault")
	for _, commitError := range []error{nil, fault} {
		result := userDeviceListDataFixture().Devices[0].UserDevice
		actor := domain.Actor{UserID: result.UserID, DeviceID: 13, SessionID: 11}
		var events []string
		rename := service.NewRenameDeviceService(func(ctx context.Context, identity domain.Actor, action func(context.Context, service.RenameDeviceTransaction) error) error {
			events = append(events, "begin")
			if identity != actor {
				t.Error("服务身份传递错误")
			}
			if err := action(ctx, renameDeviceDataTransaction{device: &result}); err != nil {
				return err
			}
			events = append(events, "commit")
			return commitError
		})
		updated, serviceError := rename.Rename(context.Background(), actor, result.ID, "  改名后的设备 🎮  ")
		data, dataError := NewRenameDeviceData(updated)
		if !reflect.DeepEqual(events, []string{"begin", "commit"}) {
			t.Fatal("服务与 DTO 衔接顺序错误")
		}
		if commitError != nil {
			if !errors.Is(serviceError, fault) || updated != nil || data != nil || !errors.Is(dataError, domain.ErrInvalidDeviceData) {
				t.Fatal("提交失败被转换为成功响应", serviceError, dataError)
			}
			continue
		}
		if serviceError != nil || dataError != nil || data == nil || data.Device.ID != "18446744073709551615" || data.Device.Name != "  改名后的设备 🎮  " {
			t.Fatal("已提交的服务结果转换失败", serviceError, dataError)
		}
		if _, err := json.Marshal(RenameDeviceResponse{Data: *data, RequestID: "28ee758a-0727-42de-ae7d-3bb33155e148"}); err != nil {
			t.Fatal("服务结果 JSON 编码失败", err)
		}
	}
}
