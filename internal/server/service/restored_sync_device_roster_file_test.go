package service

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestRestoredSyncDeviceRosterFileRoundTrip(t *testing.T) {
	original := []domain.RestoredSyncDeviceCandidate{
		{UserID: 1<<54 + 1, DeviceID: 11, State: domain.SyncDeviceState{
			Status: domain.SyncDeviceStatusActive, Generation: domain.SyncCursorGeneration{0, 0x10, 0, 0xfe}, AckSequence: 1<<54 + 3,
		}},
		{UserID: 9, DeviceID: 21, State: domain.SyncDeviceState{
			Status: domain.SyncDeviceStatusResetRequired, Generation: domain.SyncCursorGeneration{15: 0xff},
		}},
	}
	path := filepath.Join(t.TempDir(), "original-roster.json")
	if err := SaveRestoredSyncDeviceRoster(path, original); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveRestoredSyncDeviceRoster(path, []domain.RestoredSyncDeviceCandidate{}); !errors.Is(err, os.ErrExist) {
		t.Fatalf("目标已存在时应拒绝覆盖：%v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("拒绝覆盖后文件内容发生变化：%v", err)
	}
	imported, err := LoadRestoredSyncDeviceRoster(path)
	if err != nil || !slices.Equal(original, imported) {
		t.Fatalf("重开文件后的清单不符：%+v，%v", imported, err)
	}
	files, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(files) != 1 || files[0].Name() != filepath.Base(path) {
		t.Fatalf("写入或拒绝覆盖后有临时文件残留：%v，%v", files, err)
	}
}

func TestRestoredSyncDeviceRosterFileDistinguishesEmptyFromMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "original-roster.json")
	if err := SaveRestoredSyncDeviceRoster(path, nil); !errors.Is(err, domain.ErrInvalidSyncDeviceData) {
		t.Fatalf("缺少清单时应拒绝创建：%v", err)
	}
	if imported, err := LoadRestoredSyncDeviceRoster(path); imported != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("缺少文件时应返回错误：%+v，%v", imported, err)
	}
	if err := SaveRestoredSyncDeviceRoster(path, []domain.RestoredSyncDeviceCandidate{}); err != nil {
		t.Fatal(err)
	}
	imported, err := LoadRestoredSyncDeviceRoster(path)
	if err != nil || imported == nil || len(imported) != 0 {
		t.Fatalf("明确的空数组应载入为空清单：%+v，%v", imported, err)
	}
}

func TestRestoredSyncDeviceRosterFileRejectsInvalidData(t *testing.T) {
	const validRow = `{"user_id":7,"device_id":11,"status":"ACTIVE","generation":"010000000000000000000000000000ff","ack_sequence":5}`
	for _, fixture := range []struct {
		name    string
		content string
	}{
		{name: "null", content: "null"},
		{name: "truncated", content: "[" + validRow},
		{name: "second value", content: "[" + validRow + "] []"},
		{name: "short generation", content: "[" + strings.Replace(validRow, "010000000000000000000000000000ff", "01", 1) + "]"},
		{name: "long generation", content: "[" + strings.Replace(validRow, "010000000000000000000000000000ff", "010000000000000000000000000000ff00", 1) + "]"},
		{name: "bad hex", content: "[" + strings.Replace(validRow, "ff", "zz", 1) + "]"},
		{name: "zero generation", content: "[" + strings.Replace(validRow, "010000000000000000000000000000ff", strings.Repeat("0", 32), 1) + "]"},
		{name: "unknown field", content: "[" + strings.Replace(validRow, "status", "device_status", 1) + "]"},
		{name: "missing ack", content: "[" + strings.Replace(validRow, `,"ack_sequence":5`, "", 1) + "]"},
		{name: "nonzero pending ack", content: "[" + strings.Replace(validRow, "ACTIVE", "RESET_REQUIRED", 1) + "]"},
		{name: "duplicate ID", content: "[" + validRow + "," + validRow + "]"},
		{name: "unsorted", content: "[" + strings.Replace(validRow, `"device_id":11`, `"device_id":12`, 1) + "," + validRow + "]"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "invalid-roster.json")
			if err := os.WriteFile(path, []byte(fixture.content), 0600); err != nil {
				t.Fatal(err)
			}
			if imported, err := LoadRestoredSyncDeviceRoster(path); imported != nil || err == nil {
				t.Fatalf("无效文件返回了清单：%+v，%v", imported, err)
			}
		})
	}
}
