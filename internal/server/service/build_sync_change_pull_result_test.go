package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type syncChangePullResultReader struct {
	bounds domain.SyncChangeSequenceBounds
	events []domain.SyncChangeEvent
}

func (reader syncChangePullResultReader) ReadSyncChangeSequenceBounds(context.Context) (domain.SyncChangeSequenceBounds, error) {
	return reader.bounds, nil
}

func (reader syncChangePullResultReader) ListSyncChangeEvents(context.Context, uint64, uint64, int) ([]domain.SyncChangeEvent, error) {
	return reader.events, nil
}

func TestBuildSyncChangePullResult(t *testing.T) {
	// 1. 用同一合成密钥装配签发器和独立验签器。
	key := bytes.Repeat([]byte{0x34}, sha256.Size)
	signer, err := NewSyncChangeCursorSigner(key)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewSyncChangeCursorVerifier(key)
	if err != nil {
		t.Fatal(err)
	}
	generation := domain.SyncCursorGeneration{0x7f, 0, 0x18, 0, 0x34, 0, 0x56, 0, 0xff, 0, 0x33, 0, 0x15, 0, 0xee, 0x92}
	actor := domain.Actor{UserID: 7, DeviceID: 19, SessionID: 23}
	for _, scenario := range []struct {
		name                      string
		position, highWater, last uint64
		pageSize, eventCount      int
		hasMore                   bool
	}{
		{name: "more events remain", position: 17, highWater: 22, last: 19, pageSize: 2, eventCount: 2, hasMore: true},
		{name: "short final page", position: 20, highWater: 22, last: 22, pageSize: 4, eventCount: 2},
		{name: "empty page with existing sequence", position: 22, highWater: 22, last: 22, pageSize: 2},
		{name: "empty zero baseline", pageSize: 1},
		{name: "ends at maximum sequence", position: math.MaxUint64 - 1, highWater: math.MaxUint64, last: math.MaxUint64,
			pageSize: 2, eventCount: 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 2. 先通过原有读页函数绑定起点，再为该页签发续读游标。
			reader := syncChangePullResultReader{bounds: domain.SyncChangeSequenceBounds{HighWaterSequence: scenario.highWater}}
			for index := range scenario.eventCount {
				reader.events = append(reader.events, domain.SyncChangeEvent{
					Sequence: scenario.position + uint64(index) + 1, EntryID: 12, Revision: 1, EventType: "DIRECTORY_CREATED",
					Payload: []byte(`{"entry_id":"12","parent_id":"11","name":"资料","revision":"1"}`),
				})
			}
			start := SyncChangePullStart{Position: domain.SyncChangeCursorPosition{
				UserID: actor.UserID, DeviceID: actor.DeviceID, Generation: generation, Sequence: scenario.position,
			}}
			state := domain.SyncDeviceState{Status: domain.SyncDeviceStatusActive, Generation: generation}
			page, err := ReadSyncChangePageInSnapshot(t.Context(), actor, &start, state, scenario.pageSize, time.Now, reader)
			if err != nil {
				t.Fatal(err)
			}
			result, err := BuildSyncChangePullResult(t.Context(), page, signer)
			if err != nil || result == nil || result.Events == nil || len(result.Events) != scenario.eventCount ||
				result.HighWaterSequence != scenario.highWater || result.HasMore != scenario.hasMore {
				t.Fatalf("增量页结果不符：%+v，错误 %v", result, err)
			}

			// 3. 验签确认完整绑定和实际交付位置，并核对载荷为独立副本。
			position, err := verifier.Verify(actor.UserID, actor.DeviceID, result.NextCursor)
			expected := start.Position
			expected.Sequence = scenario.last
			if err != nil || position == nil || *position != expected {
				t.Fatalf("续读凭据绑定或位置不符：%+v，错误 %v", position, err)
			}
			if position, err := verifier.Verify(actor.UserID, actor.DeviceID+1, result.NextCursor); position != nil || !errors.Is(err, domain.ErrInvalidSyncChangeCursor) {
				t.Fatalf("其他设备仍能验签：%+v，错误 %v", position, err)
			}
			for index, event := range result.Events {
				original := page.Events[index]
				if event.Sequence != original.Sequence || event.EntryID != original.EntryID || event.Revision != original.Revision ||
					event.EventType != original.EventType || !bytes.Equal(event.Payload, original.Payload) {
					t.Fatalf("第 %d 条事件与读取结果不一致", index)
				}
				event.Payload[0] = '['
				result.Events[index].Sequence = 0
				if page.Events[index].Payload[0] != '{' || page.Events[index].Sequence != original.Sequence {
					t.Fatalf("第 %d 条交付事件仍与原始页共享可变数据", index)
				}
			}
		})
	}
}

func TestBuildSyncChangePullResultRejectsIncompleteResult(t *testing.T) {
	// 1. 准备合法空页和失败样本，分别验证范围、绑定、取消及签名错误。
	signer, err := NewSyncChangeCursorSigner(bytes.Repeat([]byte{0x34}, sha256.Size))
	if err != nil {
		t.Fatal(err)
	}
	page := SyncChangeReadPage{
		HighWaterSequence: 22,
		startPosition:     domain.SyncChangeCursorPosition{UserID: 7, DeviceID: 19, Generation: domain.SyncCursorGeneration{1}, Sequence: 22},
		pageSize:          2,
	}
	incomplete := page
	incomplete.HighWaterSequence++
	invalidBinding := page
	invalidBinding.startPosition.DeviceID = 0
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, scenario := range []struct {
		name      string
		ctx       context.Context
		page      *SyncChangeReadPage
		signer    *SyncChangeCursorSigner
		wantError error
	}{
		{"nil page", t.Context(), nil, signer, domain.ErrInvalidSyncChangePage},
		{"page still has unread events", t.Context(), &incomplete, signer, domain.ErrInvalidSyncChangePage},
		{"page missing read size", t.Context(), &SyncChangeReadPage{}, signer, domain.ErrInvalidSyncChangePage},
		{"invalid device binding", t.Context(), &invalidBinding, signer, domain.ErrInvalidSyncChangeCursor},
		{"canceled request", canceled, &page, signer, context.Canceled},
		{"signer unavailable", t.Context(), &page, nil, nil},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 2. 失败时整个交付结果都为空。
			result, err := BuildSyncChangePullResult(scenario.ctx, scenario.page, scenario.signer)
			if result != nil || err == nil || (scenario.wantError != nil && !errors.Is(err, scenario.wantError)) {
				t.Fatalf("期望拒绝交付，得到 %+v，错误 %v", result, err)
			}
		})
	}
}
