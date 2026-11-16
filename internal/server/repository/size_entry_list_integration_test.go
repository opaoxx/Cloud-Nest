package repository

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

type sizeEntryListSnapshotReader struct {
	*Store
	calls     *int
	afterRead func() error
}

func (reader *sizeEntryListSnapshotReader) ListUserSizeEntryCandidates(ctx context.Context, userID uint64, query domain.GlobalEntryQuery, before *domain.SizeEntryPosition) (*domain.FileEntryPage, error) {
	(*reader.calls)++
	page, err := reader.Store.ListUserSizeEntryCandidates(ctx, userID, query, before)
	if err == nil && reader.afterRead != nil {
		err = reader.afterRead()
	}
	return page, err
}

func verifySizeEntryList(t *testing.T, ctx context.Context, pool *sql.DB, store *Store) {
	t.Helper()
	// 1. 在同一快照中组合大小候选和已有祖先读取。
	makeList := func(afterRead func() error) (*service.SizeEntryListService, *int) {
		calls := 0
		listing := service.NewSizeEntryListService(func(current context.Context, action func(context.Context, service.SizeEntryListReader) error) error {
			return store.WithFileEntrySnapshot(current, func(snapshotContext context.Context, reader *Store) error {
				return action(snapshotContext, &sizeEntryListSnapshotReader{Store: reader, calls: &calls, afterRead: afterRead})
			})
		})
		return listing, &calls
	}
	actor := domain.Actor{UserID: 1, DeviceID: 19, SessionID: 17}
	masterKey := bytes.Repeat([]byte{11}, 32)
	signer, err := service.NewSizeEntryCursorSigner(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := service.NewSizeEntryCursorVerifier(masterKey)
	if err != nil {
		t.Fatal(err)
	}
	signPosition := func(t *testing.T, query domain.GlobalEntryQuery, position *domain.SizeEntryPosition) *domain.SizeEntryPosition {
		t.Helper()
		if position == nil {
			return nil
		}
		// 1. 使用正式签名器，再独立检查文本编码和大小用途 HMAC。
		original := *position
		cursor, err := signer.Sign(actor.UserID, query, *position)
		if err != nil {
			t.Fatal(err)
		}
		parts := strings.Split(cursor, ".")
		if len(parts) != 2 || len(cursor) > domain.MaxSizeEntryCursorLength || strings.ContainsAny(cursor, "=+/ \r\n") {
			t.Fatal("真实大小签名游标形状错误")
		}
		payloadBytes, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
		if err != nil || base64.RawURLEncoding.EncodeToString(payloadBytes) != parts[0] {
			t.Fatal("真实大小载荷编码错误", err)
		}
		signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
		if err != nil || len(signature) != sha256.Size || base64.RawURLEncoding.EncodeToString(signature) != parts[1] {
			t.Fatal("真实大小签名编码错误", err)
		}
		derivation := hmac.New(sha256.New, masterKey)
		derivation.Write([]byte("cloudnest/size-entry-cursor/v1"))
		messageMAC := hmac.New(sha256.New, derivation.Sum(nil))
		messageMAC.Write([]byte(parts[0]))
		if !hmac.Equal(signature, messageMAC.Sum(nil)) {
			t.Fatal("真实大小游标独立 HMAC 不符")
		}

		// 2. 独立核对七字段、筛选摘要及完整大小和 ID，并从文本还原位置。
		var fields map[string]json.RawMessage
		var payload struct {
			Version    int    `json:"version"`
			UserID     string `json:"user_id"`
			Scope      string `json:"scope"`
			Sort       string `json:"sort"`
			FilterHash string `json:"filter_hash"`
			FileSize   string `json:"file_size"`
			EntryID    string `json:"entry_id"`
		}
		filters, err := json.Marshal(struct {
			Search string `json:"q"`
			Kind   string `json:"kind"`
		}{query.Search, query.Kind})
		if err != nil {
			t.Fatal(err)
		}
		filterHash := sha256.Sum256(filters)
		if json.Unmarshal(payloadBytes, &fields) != nil || len(fields) != 7 || json.Unmarshal(payloadBytes, &payload) != nil || payload.Version != 1 || payload.UserID != strconv.FormatUint(actor.UserID, 10) || payload.Scope != query.Scope || payload.Sort != "size_desc" || payload.FilterHash != hex.EncodeToString(filterHash[:]) {
			t.Fatal("真实大小载荷字段或筛选摘要错误")
		}
		fileSize, err := strconv.ParseUint(payload.FileSize, 10, 64)
		if err != nil || payload.FileSize != strconv.FormatUint(original.FileSize, 10) {
			t.Fatal("真实大小位置数值错误", err)
		}
		entryID, err := strconv.ParseUint(payload.EntryID, 10, 64)
		if err != nil || payload.EntryID != strconv.FormatUint(original.ID, 10) || *position != original {
			t.Fatal("真实大小位置或输入保护错误", err)
		}
		resized := query
		resized.PageSize = 100
		if repeated, err := signer.Sign(actor.UserID, resized, original); err != nil || repeated != cursor {
			t.Fatal("真实大小游标页大小调整改变绑定", err)
		}
		// 3. 正式校验器与独立解码位置对照，错误绑定在续读前拒绝。
		independent := domain.SizeEntryPosition{FileSize: fileSize, ID: entryID}
		restored, err := verifier.Verify(actor.UserID, resized, cursor)
		if err != nil || restored == nil || *restored != independent || restored == position || *position != original {
			t.Fatal("正式大小校验器未还原独立位置", err)
		}
		current, err := verifier.Verify(actor.UserID, query, cursor)
		if err != nil || current == nil || current == restored || *current != independent {
			t.Fatal("大小校验结果共享位置", err)
		}
		restored.FileSize ^= 1
		if *current != independent {
			t.Fatal("大小还原结果相互影响")
		}
		changedSearch, changedKind, changedScope := query, query, query
		changedSearch.Search += " "
		changedKind.Kind = "image"
		if query.Scope == "all" {
			changedScope.Scope = "starred"
		} else {
			changedScope.Scope = "all"
		}
		for _, invalid := range []struct {
			userID uint64
			query  domain.GlobalEntryQuery
			cursor string
		}{
			{3, query, cursor}, {actor.UserID, changedSearch, cursor}, {actor.UserID, changedKind, cursor}, {actor.UserID, changedScope, cursor}, {actor.UserID, query, cursor + "A"},
		} {
			if result, err := verifier.Verify(invalid.userID, invalid.query, invalid.cursor); result != nil || !errors.Is(err, domain.ErrInvalidSizeEntryCursor) {
				t.Fatal("大小错误游标绑定通过校验", err)
			}
		}
		return current
	}
	restore := func(statement string) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Error("恢复大小列表夹具失败", err)
		}
	}
	check := func(t *testing.T, result *domain.SizeEntryList, ids []uint64, nextID uint64) {
		t.Helper()
		if result == nil || result.Entries == nil || len(result.Entries) != len(ids) || result.HasMore != (nextID != 0) || (result.NextPosition != nil) != (nextID != 0) {
			t.Fatal("大小可见列表形状或位置错误")
		}
		for index, entry := range result.Entries {
			stored, err := store.FindUserFileEntry(ctx, entry.UserID, entry.ID)
			if err != nil || stored == nil || entry.ID != ids[index] || !reflect.DeepEqual(entry, *stored) {
				t.Fatal("大小可见项与数据库事实不符", err)
			}
		}
		if nextID != 0 {
			var size uint64
			if err := pool.QueryRowContext(ctx, "SELECT file_size FROM file_entries WHERE user_id=1 AND id=?", nextID).Scan(&size); err != nil || result.NextPosition.ID != nextID || result.NextPosition.FileSize != size || !result.NextPosition.IsValid() {
				t.Fatal("大小最后消费位置与数据库不符", err)
			}
		}
	}
	checkTextPage := func(t *testing.T, query domain.GlobalEntryQuery, cursor string, ids []uint64, nextID uint64) string {
		t.Helper()
		// 1. 组合正式校验、同快照可见列表与签名，统计列表调用。
		listing, _ := makeList(nil)
		listCalls := 0
		paging := service.NewSizeEntryPageService(verifier.Verify,
			func(current context.Context, actor domain.Actor, filters domain.GlobalEntryQuery, before *domain.SizeEntryPosition) (*domain.SizeEntryList, error) {
				listCalls++
				return listing.List(current, actor, filters, before)
			}, signer.Sign)
		result, err := paging.List(ctx, actor, query, cursor)
		if err != nil || result == nil || listCalls != 1 || result.Entries == nil {
			t.Fatal("大小文本分页组合失败", err)
		}
		// 2. 对照数据库与独立协议检查，返回实际文本供下页继续读取。
		listed := &domain.SizeEntryList{Entries: result.Entries, HasMore: result.NextCursor != nil}
		if result.NextCursor == nil {
			check(t, listed, ids, nextID)
			return ""
		}
		listed.NextPosition, err = verifier.Verify(actor.UserID, query, *result.NextCursor)
		if err != nil {
			t.Fatal("大小分页返回无法校验的游标", err)
		}
		check(t, listed, ids, nextID)
		independent := signPosition(t, query, listed.NextPosition)
		repeated, err := signer.Sign(actor.UserID, query, *independent)
		if err != nil || repeated != *result.NextCursor {
			t.Fatal("大小文本分页与独立协议不符", err)
		}
		// 3. 错误用户、范围、筛选及文本在再次调用列表前拒绝。
		changedSearch, changedKind, changedScope := query, query, query
		changedSearch.Search += " "
		changedKind.Kind = "image"
		if query.Scope == "all" {
			changedScope.Scope = "starred"
		} else {
			changedScope.Scope = "all"
		}
		otherActor := actor
		otherActor.UserID = 3
		for _, invalid := range []struct {
			actor  domain.Actor
			query  domain.GlobalEntryQuery
			cursor string
		}{
			{otherActor, query, *result.NextCursor}, {actor, changedSearch, *result.NextCursor}, {actor, changedKind, *result.NextCursor},
			{actor, changedScope, *result.NextCursor}, {actor, query, *result.NextCursor + "A"},
		} {
			if failed, err := paging.List(ctx, invalid.actor, invalid.query, invalid.cursor); failed != nil || !errors.Is(err, domain.ErrInvalidSizeEntryCursor) || listCalls != 1 {
				t.Fatal("大小文本分页错误绑定进入列表", err)
			}
		}
		return *result.NextCursor
	}
	t.Run("all and starred visible pagination", func(t *testing.T) {
		for _, scope := range []string{"all", "starred"} {
			listing, _ := makeList(nil)
			query := sizeEntryCandidateQuery(2)
			query.Scope = scope
			expected := [][]uint64{{103, 101}, {105, 109}, {107, 14}, {12}}
			if scope == "starred" {
				expected = [][]uint64{{103, 101}, {107}}
			}
			var before *domain.SizeEntryPosition
			cursor := ""
			for index, ids := range expected {
				page, err := listing.List(ctx, actor, query, before)
				if err != nil {
					t.Fatal(err)
				}
				var nextID uint64
				if index+1 < len(expected) {
					nextID = ids[len(ids)-1]
				}
				check(t, page, ids, nextID)
				before = signPosition(t, query, page.NextPosition)
				cursor = checkTextPage(t, query, cursor, ids, nextID)
			}
		}
	})
	t.Run("text page resized continuation", func(t *testing.T) {
		for _, scope := range []string{"all", "starred"} {
			query := sizeEntryCandidateQuery(1)
			query.Scope = scope
			cursor := checkTextPage(t, query, "", []uint64{103}, 103)
			query.PageSize = 100
			ids := []uint64{101, 105, 109, 107, 14, 12}
			if scope == "starred" {
				ids = []uint64{101, 107}
			}
			_ = checkTextPage(t, query, cursor, ids, 0)
		}
	})
	t.Run("filters users and account state", func(t *testing.T) {
		listing, _ := makeList(nil)
		for _, fixture := range []struct {
			scope, kind, search string
			userID              uint64
			ids                 []uint64
		}{
			{"all", "folder", "", 1, []uint64{107, 14, 12}}, {"all", "document", "readme", 1, []uint64{103, 101}},
			{"starred", "document", "", 1, []uint64{103, 101}}, {"all", "", "hidden", 1, nil},
			{"all", "", "", 3, []uint64{121}}, {"all", "", "", 5, nil},
		} {
			query := sizeEntryCandidateQuery(100)
			query.Scope, query.Kind, query.Search = fixture.scope, fixture.kind, fixture.search
			identity := actor
			identity.UserID = fixture.userID
			result, err := listing.List(ctx, identity, query, nil)
			if err != nil {
				t.Fatal(err)
			}
			check(t, result, fixture.ids, 0)
			paging := service.NewSizeEntryPageService(verifier.Verify, listing.List, signer.Sign)
			page, err := paging.List(ctx, identity, query, "")
			if err != nil || page == nil || page.NextCursor != nil || !reflect.DeepEqual(page.Entries, result.Entries) {
				t.Fatal("大小文本分页筛选或用户事实错误", err)
			}
		}
		identity := actor
		identity.UserID = 999
		if result, err := listing.List(ctx, identity, sizeEntryCandidateQuery(2), nil); result != nil || !errors.Is(err, domain.ErrAccountNotFound) {
			t.Fatal("大小空页跳过账号检查", err)
		}
		paging := service.NewSizeEntryPageService(verifier.Verify, listing.List, signer.Sign)
		if page, err := paging.List(ctx, identity, sizeEntryCandidateQuery(2), ""); page != nil || !errors.Is(err, domain.ErrAccountNotFound) {
			t.Fatal("大小文本空页跳过账号检查", err)
		}
		if _, err := pool.ExecContext(ctx, "UPDATE users SET status='DISABLED' WHERE id=5"); err != nil {
			t.Fatal(err)
		}
		defer restore("UPDATE users SET status='ACTIVE' WHERE id=5")
		identity.UserID = 5
		if result, err := listing.List(ctx, identity, sizeEntryCandidateQuery(2), nil); result != nil || !errors.Is(err, domain.ErrAccountDisabled) {
			t.Fatal("大小空页禁用账号被放行", err)
		}
		if page, err := paging.List(ctx, identity, sizeEntryCandidateQuery(2), ""); page != nil || !errors.Is(err, domain.ErrAccountDisabled) {
			t.Fatal("大小文本空页禁用账号被放行", err)
		}
	})
	t.Run("single connection", func(t *testing.T) {
		pool.SetMaxOpenConns(1)
		defer pool.SetMaxOpenConns(3)
		listing, _ := makeList(nil)
		result, err := listing.List(ctx, actor, sizeEntryCandidateQuery(100), nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, result, []uint64{103, 101, 105, 109, 107, 14, 12}, 0)
	})
	t.Run("parent move snapshot", func(t *testing.T) {
		defer restore("UPDATE file_entries SET parent_id=11 WHERE id=14")
		changed := false
		listing, _ := makeList(func() error {
			if changed {
				return nil
			}
			changed = true
			_, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=16 WHERE id=14")
			return err
		})
		query := sizeEntryCandidateQuery(100)
		query.Scope = "starred"
		original, err := listing.List(ctx, actor, query, nil)
		if err != nil || original == nil || len(original.Entries) != 3 || original.Entries[0].ID != 103 {
			t.Fatal("大小父链快照未保留移动前可见项", err)
		}
		current, _ := makeList(nil)
		result, err := current.List(ctx, actor, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, result, []uint64{101, 107}, 0)
	})
	t.Run("size and membership snapshot", func(t *testing.T) {
		defer restore("UPDATE file_entries SET file_size=4096 WHERE id=103")
		defer restore("UPDATE user_entry_preferences SET starred_at=UTC_TIMESTAMP(6) WHERE user_id=1 AND entry_id=101")
		changed := false
		listing, _ := makeList(func() error {
			if changed {
				return nil
			}
			changed = true
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET file_size=16384 WHERE id=103"); err != nil {
				return err
			}
			_, err := pool.ExecContext(ctx, "UPDATE user_entry_preferences SET starred_at=NULL WHERE user_id=1 AND entry_id=101")
			return err
		})
		query := sizeEntryCandidateQuery(100)
		query.Scope = "starred"
		original, err := listing.List(ctx, actor, query, nil)
		if err != nil || original == nil || len(original.Entries) != 3 || original.Entries[0].ID != 103 || original.Entries[0].FileSize != 4096 || original.Entries[1].ID != 101 {
			t.Fatal("大小快照未保留原大小及收藏", err)
		}
		current, _ := makeList(nil)
		result, err := current.List(ctx, actor, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, result, []uint64{103, 107}, 0)
	})

	// 2. 准备一千零一项隐藏候选及两项可见文件，验证扫描预算。
	writer, err := pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	for index := 0; index < 1001; index++ {
		name := fmt.Sprintf("size-budget-hidden-%04d", index)
		if _, err := writer.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(?,1,16,?,?,'FILE','DOCUMENT',41,8192)", 3000+index, name, []byte(name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range []struct {
		entryID, size uint64
		name          string
	}{{2901, 1024, "size-budget-visible-a"}, {2903, 512, "size-budget-visible-b"}} {
		if _, err := writer.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES(?,1,12,?,?,'FILE','DOCUMENT',41,?)", entry.entryID, entry.name, []byte(entry.name), entry.size); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := writer.ExecContext(ctx, "INSERT INTO user_entry_preferences(user_id,entry_id,starred_at) SELECT user_id,id,UTC_TIMESTAMP(6) FROM file_entries WHERE user_id=1 AND (id IN (2901,2903) OR id BETWEEN 3000 AND 4000)"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, statement := range []string{
			"DELETE FROM user_entry_preferences WHERE user_id=1 AND (entry_id IN (2901,2903) OR entry_id BETWEEN 3000 AND 4000)",
			"DELETE FROM file_entries WHERE user_id=1 AND (id IN (2901,2903) OR id BETWEEN 3000 AND 4000)",
		} {
			if _, err := pool.ExecContext(cleanupContext, statement); err != nil {
				t.Error("清理大小预算夹具失败", err)
			}
		}
	}()
	for _, scope := range []string{"all", "starred"} {
		t.Run("budget empty continuation "+scope, func(t *testing.T) {
			listing, calls := makeList(nil)
			query := sizeEntryCandidateQuery(2)
			query.Scope, query.Search, query.Kind = scope, "size-budget-", "document"
			first, err := listing.List(ctx, actor, query, nil)
			if err != nil || *calls != 10 {
				t.Fatal("大小预算批次错误", err)
			}
			check(t, first, nil, 3001)
			next, err := listing.List(ctx, actor, query, signPosition(t, query, first.NextPosition))
			if err != nil || *calls != 11 {
				t.Fatal("大小预算续读错误", err)
			}
			check(t, next, []uint64{2901, 2903}, 0)
			cursor := checkTextPage(t, query, "", nil, 3001)
			_ = checkTextPage(t, query, cursor, []uint64{2901, 2903}, 0)
		})
	}
	t.Run("short page and budget edge", func(t *testing.T) {
		defer restore("UPDATE file_entries SET parent_id=16 WHERE id IN (4000,3001)")
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=12 WHERE id=4000"); err != nil {
			t.Fatal(err)
		}
		listing, _ := makeList(nil)
		query := sizeEntryCandidateQuery(3)
		query.Search = "size-budget-"
		first, err := listing.List(ctx, actor, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, first, []uint64{4000}, 3001)
		next, err := listing.List(ctx, actor, query, signPosition(t, query, first.NextPosition))
		if err != nil {
			t.Fatal(err)
		}
		check(t, next, []uint64{2901, 2903}, 0)
		cursor := checkTextPage(t, query, "", []uint64{4000}, 3001)
		_ = checkTextPage(t, query, cursor, []uint64{2901, 2903}, 0)
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=IF(id=3001,12,16) WHERE id IN (4000,3001)"); err != nil {
			t.Fatal(err)
		}
		query.PageSize = 1
		first, err = listing.List(ctx, actor, query, nil)
		if err != nil {
			t.Fatal(err)
		}
		check(t, first, []uint64{3001}, 3001)
		_ = signPosition(t, query, first.NextPosition)
		_ = checkTextPage(t, query, "", []uint64{3001}, 3001)
	})
	t.Run("exact budget end", func(t *testing.T) {
		defer restore("UPDATE file_entries SET status='ACTIVE',deleted_at=NULL WHERE id=3000")
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6) WHERE id=3000"); err != nil {
			t.Fatal(err)
		}
		listing, calls := makeList(nil)
		query := sizeEntryCandidateQuery(2)
		query.Search = "size-budget-hidden-"
		result, err := listing.List(ctx, actor, query, nil)
		if err != nil || *calls != 10 {
			t.Fatal("大小恰好预算结束错误", err)
		}
		check(t, result, nil, 0)
		_ = checkTextPage(t, query, "", nil, 0)
	})
}
