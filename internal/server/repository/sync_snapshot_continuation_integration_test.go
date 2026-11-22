package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestSyncSnapshotContinuationIntegration(t *testing.T) {
	// 1. 在独占库保存旧代次，缓存旧快照和待提交的新快照。
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	pool := syncDeviceGenerationDatabase(t, ctx)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	oldBinding := service.SyncSnapshotCacheBinding{
		SnapshotID: uuid.MustParse("052dc4f7-f4c1-46b3-a509-bf96992f6580"), UserID: 7, DeviceID: 19, PageSize: 2,
		Generation: domain.SyncCursorGeneration{0xff, 0, 7, 0, 19, 0, 21, 9, 11, 17, 21, 41, 63, 26, 33, 0xe1},
	}
	newBinding := oldBinding
	newBinding.SnapshotID[15] ^= 1
	newBinding.Generation[15] ^= 1
	if _, err := pool.ExecContext(ctx, "INSERT INTO sync_devices(device_id,user_id,cursor_generation,ack_sequence,status,last_seen_at) VALUES(19,7,?,24,'ACTIVE',UTC_TIMESTAMP(6))", oldBinding.Generation[:]); err != nil {
		t.Fatal("写入旧代次夹具失败", err)
	}
	stamp := time.Date(2026, 11, 23, 2, 0, 0, 123456789, time.UTC)
	clock := func() time.Time { return stamp }
	cache := service.NewSyncSnapshotCache(clock)
	masterKey := bytes.Repeat([]byte{0x61}, sha256.Size)
	signer, err := service.NewSyncSnapshotCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewSyncSnapshotCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	put := func(binding service.SyncSnapshotCacheBinding, sequence uint64, count int) (string, time.Time) {
		t.Helper()
		entries := make([]domain.SyncSnapshotEntry, count)
		for index := range entries {
			entries[index] = domain.SyncSnapshotEntry{
				ID: uint64(12 + index), ParentID: 11, Name: "目录" + strconv.Itoa(index), EntryType: "DIR", Revision: sequence,
			}
		}
		encoded, err := dto.EncodeSyncSnapshotEntries(entries)
		if err != nil {
			t.Fatal(err)
		}
		reservation, err := cache.ReserveBuild(ctx)
		if err != nil {
			t.Fatal(err)
		}
		expiresAt, err := cache.Put(ctx, reservation, binding, &service.BuiltSyncSnapshot{
			Header: domain.SyncSnapshotHeader{RootEntryID: 11, HighWaterSequence: sequence}, Entries: *encoded,
		})
		if err != nil {
			t.Fatal(err)
		}
		cursor, err := signer.Sign(binding, 2, expiresAt)
		if err != nil {
			t.Fatal(err)
		}
		return cursor, expiresAt
	}
	oldCursor, oldExpiresAt := put(oldBinding, 24, 5)
	newCursor, newExpiresAt := put(newBinding, 25, 3)

	// 2. 验证真实查询、签名和缓存只交付与当前已提交代次相符的页。
	cacheReads := 0
	reader := service.NewSyncSnapshotContinuationService(verifier.Verify, store.FindSyncDeviceGeneration,
		func(readContext context.Context, binding service.SyncSnapshotCacheBinding) (*service.CachedSyncSnapshot, error) {
			cacheReads++
			return cache.Get(readContext, binding)
		}, clock)
	actor := domain.Actor{UserID: oldBinding.UserID, DeviceID: oldBinding.DeviceID, SessionID: 71}
	assertPage := func(binding service.SyncSnapshotCacheBinding, cursor string, expiresAt time.Time, sequence uint64, size int, hasMore bool) {
		t.Helper()
		result, err := reader.Read(ctx, actor, binding.SnapshotID, cursor)
		if err != nil || result == nil || result.Binding != binding || result.ExpiresAt != expiresAt ||
			result.Header != (domain.SyncSnapshotHeader{RootEntryID: 11, HighWaterSequence: sequence}) || len(result.Page.Items) != size {
			t.Fatalf("续页结果不匹配，结果=%+v，错误=%v", result, err)
		}
		if (result.Page.NextPosition != nil) != hasMore || (hasMore && *result.Page.NextPosition != 4) {
			t.Fatal("续页的下一位置不正确")
		}
		for index, rawItem := range result.Page.Items {
			var item dto.SyncSnapshotEntry
			if err := json.Unmarshal(rawItem, &item); err != nil || item.EntryID != strconv.Itoa(14+index) || item.Revision != strconv.FormatUint(sequence, 10) {
				t.Fatalf("页条目顺序、版本或内容不符，条目=%+v，错误=%v", item, err)
			}
		}
	}
	assertExpired := func(binding service.SyncSnapshotCacheBinding, cursor string) {
		t.Helper()
		priorReads := cacheReads
		if result, err := reader.Read(ctx, actor, binding.SnapshotID, cursor); result != nil || !errors.Is(err, domain.ErrSyncSnapshotExpired) || cacheReads != priorReads {
			t.Fatalf("代次失配必须在访问缓存前拒绝，错误=%v", err)
		}
	}
	assertPage(oldBinding, oldCursor, oldExpiresAt, 24, 2, true)
	assertPage(oldBinding, oldCursor, oldExpiresAt, 24, 2, true)
	assertExpired(newBinding, newCursor)

	// 3. 模拟重建提交，旧快照仍在缓存时也须拒绝，当前快照返回末页。
	if _, err := pool.ExecContext(ctx, "UPDATE sync_devices SET cursor_generation=?,ack_sequence=0,status='RESET_REQUIRED' WHERE user_id=7 AND device_id=19", newBinding.Generation[:]); err != nil {
		t.Fatal("提交合成设备新代次失败", err)
	}
	assertExpired(oldBinding, oldCursor)
	assertPage(newBinding, newCursor, newExpiresAt, 25, 1, false)
}
