package dto

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestNewUserProfileDataJSON(t *testing.T) {
	base := domain.UserProfile{ID: 7, Username: "fixture", DisplayName: " 学习用户 🌱 ", Status: "ACTIVE", RootEntryID: 13, QuotaBytes: 107374182400, UsedBytes: 60, ReservedBytes: 40}
	for _, testCase := range []struct {
		name   string
		change func(*domain.UserProfile)
	}{
		{name: "normal"},
		{name: "maximum ids", change: func(profile *domain.UserProfile) { profile.ID, profile.RootEntryID = ^uint64(0), ^uint64(0) }},
		{name: "minimum ids", change: func(profile *domain.UserProfile) { profile.ID, profile.RootEntryID = 1, 1 }},
		{name: "empty display name", change: func(profile *domain.UserProfile) { profile.DisplayName = "" }},
		{name: "zero quota", change: func(profile *domain.UserProfile) {
			profile.QuotaBytes, profile.UsedBytes, profile.ReservedBytes = 0, 0, 0
		}},
		{name: "maximum safe usage", change: func(profile *domain.UserProfile) {
			profile.QuotaBytes, profile.UsedBytes, profile.ReservedBytes = maxJSONSafeInteger, maxJSONSafeInteger, 0
		}},
		{name: "maximum safe reservation", change: func(profile *domain.UserProfile) {
			profile.QuotaBytes, profile.UsedBytes, profile.ReservedBytes = maxJSONSafeInteger, 0, maxJSONSafeInteger
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			profile := base
			if testCase.change != nil {
				testCase.change(&profile)
			}
			before := profile
			data, err := NewUserProfileData(&profile)
			if err != nil || data == nil || data.User.ID != strconv.FormatUint(profile.ID, 10) || data.RootEntryID != strconv.FormatUint(profile.RootEntryID, 10) || profile != before {
				t.Fatal("DTO 转换失败、ID 丢失或修改了原资料")
			}
			encoded, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			// 使用 Number 检查精确数字文本，保留边界整数的原始数值。
			decoder := json.NewDecoder(bytes.NewReader(encoded))
			decoder.UseNumber()
			var object map[string]any
			if err := decoder.Decode(&object); err != nil {
				t.Fatal(err)
			}
			user, valid := object["user"].(map[string]any)
			if !valid || len(object) != 5 || len(user) != 3 || user["id"] != data.User.ID || user["username"] != profile.Username || user["display_name"] != profile.DisplayName || object["root_entry_id"] != data.RootEntryID {
				t.Fatal("用户子对象、字段数量或字符串字段不符合契约")
			}
			for field, expected := range map[string]uint64{"quota_bytes": profile.QuotaBytes, "used_bytes": profile.UsedBytes, "reserved_bytes": profile.ReservedBytes} {
				number, valid := object[field].(json.Number)
				if !valid || number.String() != strconv.FormatUint(expected, 10) {
					t.Fatalf("字节字段未以精确数字输出 field=%s", field)
				}
			}
			data.User.DisplayName = "修改 DTO"
			if profile != before {
				t.Fatal("修改 DTO 影响了资料原对象")
			}
		})
	}
}

func TestNewUserProfileDataInvalidInput(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		profile *domain.UserProfile
		want    error
	}{
		{name: "nil", want: domain.ErrInvalidAccountData},
		{name: "zero user", profile: &domain.UserProfile{RootEntryID: 13}, want: domain.ErrInvalidAccountData},
		{name: "zero root", profile: &domain.UserProfile{ID: 7}, want: domain.ErrInvalidAccountData},
		{name: "quota above safe limit", profile: &domain.UserProfile{ID: 7, RootEntryID: 13, QuotaBytes: maxJSONSafeInteger + 1}},
		{name: "usage above safe limit", profile: &domain.UserProfile{ID: 7, RootEntryID: 13, UsedBytes: maxJSONSafeInteger + 1}},
		{name: "reservation above safe limit", profile: &domain.UserProfile{ID: 7, RootEntryID: 13, ReservedBytes: maxJSONSafeInteger + 1}},
		{name: "maximum uint64 quota", profile: &domain.UserProfile{ID: 7, RootEntryID: 13, QuotaBytes: ^uint64(0)}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			data, err := NewUserProfileData(testCase.profile)
			if data != nil || err == nil || testCase.want != nil && !errors.Is(err, testCase.want) {
				t.Fatal("无效资料未返回固定错误与空结果")
			}
			if strings.Contains(err.Error(), "9007199254740992") || strings.Contains(err.Error(), "18446744073709551615") {
				t.Fatal("错误消息回显了实际字节数")
			}
		})
	}
}
