package service_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestBuildCreateSyncSnapshotRequestHashVectors(t *testing.T) {
	// 1. 对照 .NET SHA-256 计算的固定摘要，核对操作名、字段顺序与完整设备 ID。
	for _, fixture := range []struct {
		name, wantHash string
		deviceID       uint64
		pageSize       int
	}{
		{"default size", "be6a84ab035f6e95ab90d2ccafc015be10ff37d71c62d4dc5d6bb709cf874f5c", 19, 50},
		{"minimum size", "f0e74709ff71c0aade5aef0ff4d8b45ebe85919367c843e40a7aacad39cada64", 1, 1},
		{"maximum device and size", "1fa93472165eca0c57be76541b537ee09fba75b67caa247da78631c1cabb47f6", math.MaxUint64, 100},
		{"device above safe JSON number range", "ff1266a40422b10f8a6133e6839e1b97d55135c432bded81ba43acbbab658d1e", 1<<53 + 1, 50},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			actual, err := service.BuildCreateSyncSnapshotRequestHash(fixture.deviceID, fixture.pageSize)
			if err != nil || hex.EncodeToString(actual[:]) != fixture.wantHash {
				t.Fatalf("请求摘要为 %x，错误 %v，期望 %s", actual, err, fixture.wantHash)
			}
		})
	}
}

func TestBuildCreateSyncSnapshotRequestHashFromDTO(t *testing.T) {
	// 1. 将省略、显式及转义字段名的等价正文通过上一步 DTO。
	const deviceID = 1<<53 + 1
	want, err := service.BuildCreateSyncSnapshotRequestHash(deviceID, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{}`, `{"page_size":50}`, `{ "page_siz\u0065" : 50 }`} {
		var request dto.CreateSyncSnapshotRequest
		if err := json.Unmarshal([]byte(body), &request); err != nil {
			t.Fatal(err)
		}
		pageSize, fieldError := dto.ParseCreateSyncSnapshotPageSize(request)
		if fieldError != nil {
			t.Fatalf("等价正文解析失败 %+v", fieldError)
		}
		actual, err := service.BuildCreateSyncSnapshotRequestHash(deviceID, pageSize)
		if err != nil || actual != want {
			t.Fatalf("正文 %s 得到不同摘要 %x，错误 %v", body, actual, err)
		}
	}

	// 2. 仅改变设备 ID 或页大小，确认分别得到不同摘要。
	otherDevice, deviceError := service.BuildCreateSyncSnapshotRequestHash(deviceID-1, 50)
	otherSize, sizeError := service.BuildCreateSyncSnapshotRequestHash(deviceID, 51)
	if deviceError != nil || otherDevice == want {
		t.Fatalf("仅改变设备 ID 后摘要为 %x，错误 %v", otherDevice, deviceError)
	}
	if sizeError != nil || otherSize == want {
		t.Fatalf("仅改变页大小后摘要为 %x，错误 %v", otherSize, sizeError)
	}
}

func TestBuildCreateSyncSnapshotRequestHashRejectsInvalidParameters(t *testing.T) {
	for _, fixture := range []struct {
		name     string
		deviceID uint64
		pageSize int
	}{
		{"zero device", 0, 50},
		{"negative size", 19, -1},
		{"zero size", 19, 0},
		{"size above limit", 19, 101},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			actual, err := service.BuildCreateSyncSnapshotRequestHash(fixture.deviceID, fixture.pageSize)
			if err == nil || actual != [sha256.Size]byte{} {
				t.Fatalf("无效参数得到摘要 %x，错误 %v", actual, err)
			}
		})
	}
}
