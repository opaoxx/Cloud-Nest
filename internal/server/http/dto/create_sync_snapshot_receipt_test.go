package dto

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

const (
	snapshotReceiptID         = "052dc4f7-f4c1-46b3-a509-bf96992f6580"
	snapshotReceiptDevice     = "18446744073709551615"
	snapshotReceiptGeneration = "00800102030405060708090a0b0c0dff"
	snapshotReceiptJSON       = `{"version":1,"snapshot_id":"` + snapshotReceiptID + `","device_id":"` +
		snapshotReceiptDevice + `","generation":"` + snapshotReceiptGeneration + `"}`
)

func syncSnapshotReceiptFixture() domain.SyncSnapshotReceiptLocator {
	return domain.SyncSnapshotReceiptLocator{
		SnapshotID: uuid.MustParse(snapshotReceiptID),
		DeviceID:   math.MaxUint64,
		Generation: domain.SyncCursorGeneration{0, 0x80, 1, 2, 3, 4, 5, 6, 7, 8, 9, 0x0a, 0x0b, 0x0c, 0x0d, 0xff},
	}
}

func TestCreateSyncSnapshotReceiptRoundTrip(t *testing.T) {
	// 1. 核对定位数据只编码约定的四个字段。
	locator := syncSnapshotReceiptFixture()
	encoded, err := EncodeCreateSyncSnapshotReceipt(locator)
	if err != nil || string(encoded) != snapshotReceiptJSON {
		t.Fatalf("回执编码为 %s，错误 %v", encoded, err)
	}

	// 2. 回读原编码以及模拟存储层调整顺序和空白后的 JSON。
	for _, body := range []json.RawMessage{
		encoded,
		json.RawMessage(` { "version": 1, "device_id": "` + snapshotReceiptDevice + `", "generation": "` +
			snapshotReceiptGeneration + `", "snapshot_id": "` + snapshotReceiptID + `" } `),
	} {
		decoded, err := DecodeCreateSyncSnapshotReceipt(body)
		if err != nil || decoded == nil || *decoded != locator {
			t.Fatalf("定位数据回读为 %+v，错误 %v", decoded, err)
		}
	}
}

func TestCreateSyncSnapshotReceiptRejectsInvalidLocator(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*domain.SyncSnapshotReceiptLocator)
	}{
		{"zero device", func(locator *domain.SyncSnapshotReceiptLocator) { locator.DeviceID = 0 }},
		{"nil UUID", func(locator *domain.SyncSnapshotReceiptLocator) { locator.SnapshotID = uuid.Nil }},
		{"wrong UUID version", func(locator *domain.SyncSnapshotReceiptLocator) { locator.SnapshotID[6] = 0x56 }},
		{"wrong UUID variant", func(locator *domain.SyncSnapshotReceiptLocator) { locator.SnapshotID[8] = 0xc5 }},
		{"zero generation", func(locator *domain.SyncSnapshotReceiptLocator) { locator.Generation = domain.SyncCursorGeneration{} }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			locator := syncSnapshotReceiptFixture()
			testCase.mutate(&locator)
			encoded, err := EncodeCreateSyncSnapshotReceipt(locator)
			if encoded != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
				t.Fatalf("非法定位数据编码为 %s，错误 %v", encoded, err)
			}
		})
	}
}

func TestCreateSyncSnapshotReceiptRejectsInvalidBody(t *testing.T) {
	const versionField = `"version":1`
	const idField = `"snapshot_id":"` + snapshotReceiptID + `"`
	const deviceField = `"device_id":"` + snapshotReceiptDevice + `"`
	for _, testCase := range []struct {
		name, old, replacement string
	}{
		{"absent body", snapshotReceiptJSON, ""},
		{"non-object body", snapshotReceiptJSON, "null"},
		{"trailing object", snapshotReceiptJSON, snapshotReceiptJSON + "{}"},
		{"missing version", versionField + ",", ""},
		{"unsupported version", versionField, `"version":2`},
		{"null version", versionField, `"version":null`},
		{"quoted version", versionField, `"version":"1"`},
		{"unknown field", versionField, versionField + `,"page_size":50`},
		{"duplicate version", versionField, versionField + "," + versionField},
		{"duplicate escaped key", versionField, versionField + `,"versio\u006e":1`},
		{"case variant field", `"snapshot_id"`, `"Snapshot_ID"`},
		{"uppercase UUID", snapshotReceiptID, strings.ToUpper(snapshotReceiptID)},
		{"wrong UUID version", "-46b3-", "-56b3-"},
		{"wrong UUID variant", "-a509-", "-c509-"},
		{"null UUID", idField, `"snapshot_id":null`},
		{"numeric device", deviceField, `"device_id":` + snapshotReceiptDevice},
		{"zero device", snapshotReceiptDevice, "0"},
		{"leading zero device", snapshotReceiptDevice, "019"},
		{"overflowing device", snapshotReceiptDevice, "18446744073709551616"},
		{"short generation", snapshotReceiptGeneration, snapshotReceiptGeneration[2:]},
		{"uppercase generation", snapshotReceiptGeneration, strings.ToUpper(snapshotReceiptGeneration)},
		{"non-hex generation", snapshotReceiptGeneration, "xx" + snapshotReceiptGeneration[2:]},
		{"zero generation", snapshotReceiptGeneration, strings.Repeat("0", 32)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			body := json.RawMessage(strings.Replace(snapshotReceiptJSON, testCase.old, testCase.replacement, 1))
			locator, err := DecodeCreateSyncSnapshotReceipt(body)
			if locator != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
				t.Fatalf("非法回执解码为 %+v，错误 %v", locator, err)
			}
		})
	}
}
