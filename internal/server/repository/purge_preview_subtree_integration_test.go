package repository

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestListUserPurgePreviewSubtreeIntegration(t *testing.T) {
	// 1. 在独占库准备回收目标、各状态后代、旁支和另一用户。
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	pool, store, actor := trashEntryTestDatabase(t, ctx)
	for _, statement := range []string{
		"UPDATE file_entries SET status='TRASHED', deleted_at=UTC_TIMESTAMP(6) WHERE user_id=1 AND id=13",
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at,purge_started_at)
		  VALUES (9,1,14,'回收后代',CAST('回收后代' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6),NULL),
		         (17,1,14,'清理中后代',CAST('清理中后代' AS BINARY),'DIR','PURGING',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6))`,
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,file_size,status,deleted_at,purged_at,revision)
		  VALUES (8,1,9,'保留墓碑.md',CAST('保留墓碑.md' AS BINARY),'FILE','DOCUMENT',1024,'PURGED',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6),7)`,
		`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,status,deleted_at)
		  VALUES (10,1,9,'共享对象.md',CAST('共享对象.md' AS BINARY),'FILE','DOCUMENT',41,2048,'TRASHED',UTC_TIMESTAMP(6))`,
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("准备清理子树夹具失败", err)
		}
	}

	// 2. 装配真实快照和签发器，验证预检返回的数据及凭证。
	masterKey := bytes.Repeat([]byte{9}, sha256.Size)
	signer, err := service.NewPurgePreviewSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewPurgePreviewVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	issuedAt := time.Date(2026, 11, 20, 4, 30, 0, 123456000, time.UTC)
	clock := func() time.Time { return issuedAt }
	previewService := service.NewPurgePreviewService(func(ctx context.Context, action func(context.Context, service.PurgePreviewReader) error) error {
		return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, snapshotStore *Store) error {
			return action(snapshotContext, snapshotStore)
		})
	}, signer, clock)
	recheck := func(entryID uint64, credential domain.PurgePreviewCredential, checkAt time.Time) (*domain.FileEntry, error) {
		var target *domain.FileEntry
		if err := store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			var recheckError error
			target, recheckError = service.RecheckPurgePreview(txContext, actor, entryID, credential.PreviewDigest, txStore, verifier, func() time.Time { return checkAt })
			return recheckError
		}); err != nil {
			return nil, err
		}
		return target, nil
	}
	const originalFingerprint = "bc153a0f7df0fe9c078dab05127989f8c445b325f611c4989b3a506c2e5e921e"
	t.Run("all states ordered by ID", func(t *testing.T) {
		want := []domain.PurgePreviewSubtreeEntry{
			{ID: 8, EntryType: "FILE", Status: "PURGED", Revision: 7, FileSize: 1024},
			{ID: 9, EntryType: "DIR", Status: "TRASHED", Revision: 1},
			{ID: 10, EntryType: "FILE", Status: "TRASHED", Revision: 1, FileSize: 2048},
			{ID: 14, EntryType: "DIR", Status: "TRASHED", Revision: 1},
			{ID: 15, EntryType: "FILE", Status: "ACTIVE", Revision: 5, FileSize: 2048},
			{ID: 17, EntryType: "DIR", Status: "PURGING", Revision: 1},
		}
		actual, err := store.ListUserPurgePreviewSubtree(ctx, actor.UserID, 14)
		if err != nil || !reflect.DeepEqual(actual, want) {
			t.Fatalf("子树记录为 %+v，错误 %v", actual, err)
		}
		// 两个文件引用对象 41，各计一次逻辑大小。已结算墓碑只参与指纹。
		preview, err := previewService.Preview(ctx, actor, 14)
		if err != nil || preview == nil || preview.EntryID != 14 || preview.Revision != 1 ||
			preview.Summary != (domain.PurgePreviewSummary{FileCount: 2, DirectoryCount: 3, LogicalBytes: 4096}) ||
			!preview.Credential.ExpiresAt.Equal(issuedAt.Add(5*time.Minute)) {
			t.Fatalf("预检结果为 %+v，错误 %v", preview, err)
		}
		data, err := dto.NewPurgePreviewData(preview)
		if err != nil || data == nil {
			t.Fatal("无法转换预检结果", data, err)
		}
		wire, err := json.Marshal(data)
		wantData := fmt.Sprintf(`{"entry_id":"14","revision":"1","file_count":2,"directory_count":3,"logical_bytes":"4096","preview_digest":%q,"expires_at":"2026-11-20T04:35:00.123456Z"}`,
			preview.Credential.PreviewDigest)
		if err != nil || string(wire) != wantData {
			t.Fatalf("真实预检 JSON 为 %s，期望 %s，错误 %v", wire, wantData, err)
		}
		verified, err := verifier.Verify(actor.UserID, 14, data.PreviewDigest, issuedAt.Add(time.Minute))
		if err != nil || fmt.Sprintf("%x", verified) != originalFingerprint {
			t.Fatalf("校验得到的实际子树指纹为 %x，错误 %v", verified, err)
		}
		target, err := recheck(14, preview.Credential, issuedAt.Add(time.Minute))
		if err != nil || target == nil || target.ID != 14 || target.Revision != 1 || target.Status != "TRASHED" {
			t.Fatal("用户事务未确认相同的子树范围", target, err)
		}
		if target, err := recheck(14, preview.Credential, preview.Credential.ExpiresAt); target != nil || !errors.Is(err, domain.ErrPurgePreviewChanged) {
			t.Fatal("到期时应要求重新预检", target, err)
		}
	})

	t.Run("file target and isolation", func(t *testing.T) {
		entries, err := store.ListUserPurgePreviewSubtree(ctx, actor.UserID, 13)
		want := []domain.PurgePreviewSubtreeEntry{{ID: 13, EntryType: "FILE", Status: "TRASHED", Revision: 3, FileSize: 2048}}
		if err != nil || !reflect.DeepEqual(entries, want) {
			t.Fatal("文件目标应只返回自身", entries, err)
		}
		preview, err := previewService.Preview(ctx, actor, 13)
		if err != nil || preview == nil || preview.Revision != 3 ||
			preview.Summary != (domain.PurgePreviewSummary{FileCount: 1, LogicalBytes: 2048}) {
			t.Fatal("单文件预检范围不符", preview, err)
		}
		for _, entryID := range []uint64{21, 999} {
			if entries, err := store.ListUserPurgePreviewSubtree(ctx, actor.UserID, entryID); entries != nil || !errors.Is(err, domain.ErrFileEntryNotFound) {
				t.Fatal("跨用户或缺失目标未被拒绝", entries, err)
			}
			if result, err := previewService.Preview(ctx, actor, entryID); result != nil || !errors.Is(err, domain.ErrFileEntryNotFound) {
				t.Fatal("预检跨用户或缺失目标应返回不存在", result, err)
			}
		}
		cancelled, stop := context.WithCancel(ctx)
		stop()
		if entries, err := store.ListUserPurgePreviewSubtree(cancelled, actor.UserID, 14); entries != nil || !errors.Is(err, context.Canceled) {
			t.Fatal("取消未传播", entries, err)
		}
	})

	t.Run("consistent snapshot across concurrent insert", func(t *testing.T) {
		pool.SetMaxOpenConns(2)
		defer pool.SetMaxOpenConns(1)
		concurrentPreview := service.NewPurgePreviewService(func(ctx context.Context, action func(context.Context, service.PurgePreviewReader) error) error {
			return store.WithFileEntrySnapshot(ctx, func(snapshotContext context.Context, snapshotStore *Store) error {
				reader := &fileEntryPathChangingReader{Store: snapshotStore, afterRead: func(entryID uint64) error {
					if entryID != 14 {
						return nil
					}
					return store.WithUserTransaction(snapshotContext, actor, func(txContext context.Context, txStore *Store) error {
						return txStore.database.WithContext(txContext).Exec(
							"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(19,1,14,'并发加入',CAST('并发加入' AS BINARY),'DIR')",
						).Error
					})
				}}
				return action(snapshotContext, reader)
			})
		}, signer, clock)

		// 已读目标后另一连接新增后代，本次预检仍使用已建立的快照。
		before, err := concurrentPreview.Preview(ctx, actor, 14)
		if err != nil || before == nil || before.Summary.DirectoryCount != 3 {
			t.Fatal("本次预检应使用新增前的范围", before, err)
		}
		beforeFingerprint, err := verifier.Verify(actor.UserID, 14, before.Credential.PreviewDigest, issuedAt)
		if err != nil || fmt.Sprintf("%x", beforeFingerprint) != originalFingerprint {
			t.Fatalf("本次凭证应绑定新增前的指纹，实际 %x，错误 %v", beforeFingerprint, err)
		}
		after, err := previewService.Preview(ctx, actor, 14)
		if err != nil || after == nil || after.Revision != before.Revision || after.Summary.DirectoryCount != 4 {
			t.Fatal("新快照应反映并发新增，目标版本保持相同", after, err)
		}
		afterFingerprint, err := verifier.Verify(actor.UserID, 14, after.Credential.PreviewDigest, issuedAt)
		if err != nil || afterFingerprint == beforeFingerprint {
			t.Fatal("新快照指纹应反映并发新增", err)
		}
		if target, err := recheck(14, before.Credential, issuedAt); target != nil || !errors.Is(err, domain.ErrPurgePreviewChanged) {
			t.Fatal("用户锁内的确认应识别新增后代并拒绝旧范围", target, err)
		}
		if target, err := recheck(14, after.Credential, issuedAt); err != nil || target == nil || target.Revision != before.Revision {
			t.Fatal("新凭证应通过复查并返回同一个目标版本", target, err)
		}
	})

	// 3. 验证真实 10000 条范围及额外墓碑触发的超限。
	t.Run("ten thousand and one more retained tombstone", func(t *testing.T) {
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(60,1,11,'宽子树',CAST('宽子树' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))"); err != nil {
			t.Fatal(err)
		}
		childCount := domain.MaxFileEntrySubtreeNodes - 1
		for start := 0; start < childCount; start += 500 {
			end := min(start+500, childCount)
			parameters := make([]any, 0, (end-start)*3)
			values := make([]string, 0, end-start)
			for index := start; index < end; index++ {
				entryID := 1000 + index
				name := fmt.Sprintf("node-%d", entryID)
				values = append(values, "(?,1,60,?,?,'DIR')")
				parameters = append(parameters, entryID, name, []byte(name))
			}
			if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES "+strings.Join(values, ","), parameters...); err != nil {
				t.Fatal(err)
			}
		}
		entries, err := store.ListUserPurgePreviewSubtree(ctx, actor.UserID, 60)
		if err != nil || len(entries) != domain.MaxFileEntrySubtreeNodes || entries[0].ID != 60 ||
			entries[len(entries)-1].ID != uint64(999+childCount) ||
			!slices.IsSortedFunc(entries, func(first, second domain.PurgePreviewSubtreeEntry) int {
				return cmp.Compare(first.ID, second.ID)
			}) {
			t.Fatal("10000 条范围或顺序不正确", len(entries), err)
		}
		if _, err := service.BuildPurgePreviewSubtreeFingerprint(entries); err != nil {
			t.Fatal("无法为 10000 条保留记录生成指纹", err)
		}
		preview, err := previewService.Preview(ctx, actor, 60)
		if err != nil || preview == nil {
			t.Fatal("达到 10000 条的子树应能签发凭证", err)
		}
		if _, err := pool.ExecContext(ctx, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at,purged_at)
		  VALUES(20000,1,60,'额外墓碑',CAST('额外墓碑' AS BINARY),'DIR','PURGED',UTC_TIMESTAMP(6),UTC_TIMESTAMP(6))`); err != nil {
			t.Fatal(err)
		}
		if entries, err := store.ListUserPurgePreviewSubtree(ctx, actor.UserID, 60); entries != nil || !errors.Is(err, domain.ErrFileEntrySubtreeTooLarge) {
			t.Fatal("10001 条范围应整体拒绝", len(entries), err)
		}
		if result, err := previewService.Preview(ctx, actor, 60); result != nil || !errors.Is(err, domain.ErrFileEntrySubtreeTooLarge) {
			t.Fatal("超限范围应拒绝预检", result, err)
		}
		if target, err := recheck(60, preview.Credential, issuedAt); target != nil || !errors.Is(err, domain.ErrPurgePreviewChanged) {
			t.Fatal("范围超限后确认应提示重新预检", target, err)
		}
	})

	// 4. 拒绝循环、文件充当父目录和超过相对深度范围的结果。
	t.Run("invalid subtree structure", func(t *testing.T) {
		for _, statement := range []string{
			"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(70,1,11,'循环起点',CAST('循环起点' AS BINARY),'DIR'),(71,1,70,'循环后代',CAST('循环后代' AS BINARY),'DIR'),(72,1,13,'挂在文件下',CAST('挂在文件下' AS BINARY),'DIR')",
			"UPDATE file_entries SET parent_id=71 WHERE user_id=1 AND id=70",
		} {
			if _, err := pool.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
		parentID := uint64(11)
		for depth := 0; depth <= domain.MaxFileEntryDepth+1; depth++ {
			entryID := 80 + uint64(depth)
			if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,1,?,'深层',CAST('深层' AS BINARY),'DIR')", entryID, parentID); err != nil {
				t.Fatal(err)
			}
			parentID = entryID
		}
		for _, entryID := range []uint64{70, 13, 80} {
			if entries, err := store.ListUserPurgePreviewSubtree(ctx, actor.UserID, entryID); entries != nil || !errors.Is(err, domain.ErrInvalidFileEntryData) {
				t.Fatal("异常子树未被拒绝", entryID, entries, err)
			}
		}
	})

	t.Run("maximum unsigned fields", func(t *testing.T) {
		maximum := uint64(math.MaxUint64)
		if _, err := pool.ExecContext(ctx, `INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision)
		  VALUES(?,1,11,'最大字段.md',CAST('最大字段.md' AS BINARY),'FILE','DOCUMENT',41,?,?)`, maximum, maximum, maximum); err != nil {
			t.Fatal(err)
		}
		entries, err := store.ListUserPurgePreviewSubtree(ctx, actor.UserID, maximum)
		want := []domain.PurgePreviewSubtreeEntry{{ID: maximum, EntryType: "FILE", Status: "ACTIVE", Revision: maximum, FileSize: maximum}}
		if err != nil || !reflect.DeepEqual(entries, want) {
			t.Fatal("无符号字段未完整保留", entries, err)
		}
		summary, err := service.SummarizePurgePreviewSubtree(entries)
		if err != nil || summary != (domain.PurgePreviewSummary{FileCount: 1, LogicalBytes: maximum}) {
			t.Fatal("最大逻辑用量的汇总不符", summary, err)
		}
	})
}
