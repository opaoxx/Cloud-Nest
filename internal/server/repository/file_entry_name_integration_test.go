package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestFindUserActiveFileEntryByNameIntegration(t *testing.T) {
	// 1. 复用正式表及独占库，建立不同用户、父目录和生命周期的同名记录。
	pool := userProfileDatabase(t)
	pool.SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	for _, statement := range []string{
		"INSERT INTO users(id,username,display_name,password_hash,status) VALUES(1,'fixture_one','名称查询用户','fixture_hash','ACTIVE'),(2,'fixture_two','禁用用户','fixture_hash','DISABLED'),(3,'fixture_three','其他用户','fixture_hash','ACTIVE')",
		"INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(41,1,'MINIO','fixture-bucket','fixture-object',2048)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(11,1,NULL,'',X'','DIR'),(22,2,NULL,'',X'','DIR'),(33,3,NULL,'',X'','DIR'),(12,1,11,'资料',CAST('资料' AS BINARY),'DIR'),(14,1,12,'ReadMe.MD',X'726561646d652e6d64','DIR'),(34,3,33,'ReadMe.MD',X'726561646d652e6d64','DIR'),(23,2,22,'ReadMe.MD',X'726561646d652e6d64','DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision) VALUES(13,1,11,'ReadMe.MD',X'726561646d652e6d64','FILE','DOCUMENT',41,2048,3)",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,deleted_at) VALUES(15,1,11,'回收目录',CAST('回收目录' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6)),(20,1,11,'历史',CAST('历史' AS BINARY),'DIR','TRASHED',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(16,1,15,'ReadMe.MD',X'726561646d652e6d64','DIR'),(40,1,11,'before.md',X'6265666f72652e6d64','DIR')",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,purge_started_at) VALUES(21,1,11,'历史',CAST('历史' AS BINARY),'DIR','PURGING',UTC_TIMESTAMP(6))",
		"INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,status,purged_at) VALUES(24,1,11,'历史',CAST('历史' AS BINARY),'DIR','PURGED',UTC_TIMESTAMP(6))",
	} {
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal("创建活动同名查询夹具失败", err)
		}
	}
	stamp := time.Date(2026, 11, 15, 2, 30, 0, 123456000, time.UTC)
	if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET created_at=?,modified_at=?,updated_at=?", stamp, stamp.Add(time.Second), stamp.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	keyFor := func(name string) []byte {
		t.Helper()
		key, err := service.BuildEntryNameKey(name)
		if err != nil {
			t.Fatal("生成联测名称键失败", err)
		}
		return key
	}
	check := func(t *testing.T, reader *Store, readContext context.Context, userID, parentID uint64, key []byte, entryID uint64) *domain.FileEntry {
		t.Helper()
		entry, err := reader.FindUserActiveFileEntryByName(readContext, userID, parentID, key)
		if entryID == 0 {
			if entry != nil || !errors.Is(err, domain.ErrFileEntryNotFound) {
				t.Fatal("预期缺失的活动名称被命中或错误类别不符", err)
			}
			return nil
		}
		if err != nil || entry == nil || entry.ID != entryID || entry.UserID != userID || entry.ParentID == nil || *entry.ParentID != parentID || entry.Status != "ACTIVE" || entry.Revision == 0 {
			t.Fatal("活动同名项标识、范围或状态错误", err)
		}
		return entry
	}

	// 2. 核对基础字段、独立副本、用户/父目录隔离及已有唯一索引。
	t.Run("fields copies scope and read only", func(t *testing.T) {
		var beforeCount int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM file_entries").Scan(&beforeCount); err != nil {
			t.Fatal(err)
		}
		parentID := uint64(11)
		want := domain.FileEntry{ID: 13, UserID: 1, ParentID: &parentID, Name: "ReadMe.MD", EntryType: "FILE", FileSize: 2048, Revision: 3, Status: "ACTIVE", CreatedAt: stamp, ModifiedAt: stamp.Add(time.Second), UpdatedAt: stamp.Add(2 * time.Second)}
		key := keyFor("  README.md  ")
		first := check(t, store, ctx, 1, 11, key, 13)
		second := check(t, store, ctx, 1, 11, key, 13)
		if !reflect.DeepEqual(first, &want) || !reflect.DeepEqual(second, &want) || first == second || first.ParentID == second.ParentID {
			t.Fatal("十一项基础字段或独立副本错误")
		}
		first.Name, *first.ParentID = "修改本地结果", 999
		if !reflect.DeepEqual(second, &want) || !reflect.DeepEqual(check(t, store, ctx, 1, 11, key, 13), &want) {
			t.Fatal("本地副本修改影响其他结果或数据库")
		}
		check(t, store, ctx, 1, 12, key, 14)
		check(t, store, ctx, 3, 33, key, 34)
		for _, scope := range [][2]uint64{{1, 33}, {3, 11}, {1, 999}, {99, 11}} {
			check(t, store, ctx, scope[0], scope[1], key, 0)
		}
		check(t, store, ctx, 1, 11, keyFor("不存在"), 0)
		var rows int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM file_entries").Scan(&rows); err != nil || rows != beforeCount {
			t.Fatal("只读查询改变了目录项数量", err)
		}
	})
	t.Run("unique name index lookup", func(t *testing.T) {
		var content []byte
		if err := pool.QueryRowContext(ctx, "EXPLAIN FORMAT=JSON SELECT id FROM file_entries WHERE user_id=? AND parent_id=? AND active_name_key=? AND status=? LIMIT 1", 1, 11, keyFor("readme.md"), "ACTIVE").Scan(&content); err != nil {
			t.Fatal(err)
		}
		var plan struct {
			QueryBlock struct {
				Table struct {
					Key string `json:"key"`
				} `json:"table"`
			} `json:"query_block"`
		}
		if err := json.Unmarshal(content, &plan); err != nil || plan.QueryBlock.Table.Key != "uk_entries_name" {
			t.Fatal("活动同名查询未使用已有活动名称唯一索引", err)
		}
	})

	// 3. 用实际名称键验证 Unicode 等价、大小写展开及二进制精确比较。
	t.Run("name rules and unique constraint", func(t *testing.T) {
		for index, names := range [][2]string{
			{"Café.txt", "CAFE\u0301.TXT"},
			{"Straße", "STRASSE"},
			{"ΟΣ", "ος"},
			{"각", "\u1100\u1161\u11a8"},
			{"oﬃce", "OFFICE"},
			{"q" + strings.Repeat("\u0301", 31) + "\u0323", "Q\u0323" + strings.Repeat("\u0301", 31)},
			{"100%_ ' .txt", "100%_ ' .TXT"},
		} {
			entryID := uint64(50 + index)
			key := keyFor(names[0])
			if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,1,11,?,?,'DIR')", entryID, names[0], key); err != nil {
				t.Fatal(err)
			}
			found := check(t, store, ctx, 1, 11, keyFor(names[1]), entryID)
			if found.Name != names[0] {
				t.Fatal("同名查询改变了保存的展示名")
			}
			_, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,1,11,?,?,'DIR')", entryID+100, names[1], keyFor(names[1]))
			var mysqlError *mysql.MySQLError
			if !errors.As(err, &mysqlError) || mysqlError.Number != 1062 || !strings.Contains(mysqlError.Message, "uk_entries_name") {
				t.Fatal("等价名称未受到活动名称唯一键约束", err)
			}
		}
		for _, name := range []string{"cafe.txt", "Ａ", "①", "x' OR 1=1 --", "%", "_"} {
			check(t, store, ctx, 1, 11, keyFor(name), 0)
		}
		for index, names := range [][2]string{{"a", "Ａ"}, {"1", "①"}} {
			for variant, name := range names {
				entryID := uint64(70 + index*2 + variant)
				if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,1,11,?,?,'DIR')", entryID, name, keyFor(name)); err != nil {
					t.Fatal(err)
				}
				check(t, store, ctx, 1, 11, keyFor(name), entryID)
			}
			check(t, store, ctx, 1, 11, keyFor(names[0]), uint64(70+index*2))
		}
		for _, suffix := range [][]byte{[]byte(" "), {0}} {
			check(t, store, ctx, 1, 11, append(keyFor("readme.md"), suffix...), 0)
		}
		for index, key := range [][]byte{bytes.Repeat([]byte{0xff}, 768), {'a', 0, 0xff}} {
			entryID := uint64(80 + index)
			if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,1,11,'二进制键边界',?,'DIR')", entryID, key); err != nil {
				t.Fatal(err)
			}
			check(t, store, ctx, 1, 11, key, entryID)
		}
	})

	// 4. 历史同名记录释放活动名称，账号和祖先可见性由上层服务继续检查。
	t.Run("historical names and raw visibility facts", func(t *testing.T) {
		key := keyFor("历史")
		check(t, store, ctx, 1, 11, key, 0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(25,1,11,'历史',?,'DIR')", key); err != nil {
			t.Fatal(err)
		}
		check(t, store, ctx, 1, 11, key, 25)
		if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6) WHERE id=25"); err != nil {
			t.Fatal(err)
		}
		check(t, store, ctx, 1, 11, key, 0)
		check(t, store, ctx, 1, 15, keyFor("readme.md"), 16)
		check(t, store, ctx, 2, 22, keyFor("readme.md"), 23)
	})

	// 5. 复用一致性读取，在未提交写入及已有只读快照中保持正确视图。
	t.Run("consistent read while rename uncommitted", func(t *testing.T) {
		transaction, err := pool.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer transaction.Rollback()
		if _, err := transaction.ExecContext(ctx, "UPDATE file_entries SET name='after.md',name_key=?,revision=revision+1 WHERE id=40", keyFor("after.md")); err != nil {
			t.Fatal(err)
		}
		readContext, release := context.WithTimeout(ctx, 500*time.Millisecond)
		defer release()
		check(t, store, readContext, 1, 11, keyFor("before.md"), 40)
		check(t, store, readContext, 1, 11, keyFor("after.md"), 0)
		if err := transaction.Commit(); err != nil {
			t.Fatal(err)
		}
		check(t, store, ctx, 1, 11, keyFor("before.md"), 0)
		if current := check(t, store, ctx, 1, 11, keyFor("after.md"), 40); current.Name != "after.md" || current.Revision != 2 {
			t.Fatal("提交改名后未取得新名称及版本")
		}
	})
	t.Run("shared snapshot across committed move and rename", func(t *testing.T) {
		if err := store.WithFileEntrySnapshot(ctx, func(txContext context.Context, txStore *Store) error {
			first := check(t, txStore, txContext, 1, 11, keyFor("after.md"), 40)
			if _, err := pool.ExecContext(ctx, "UPDATE file_entries SET parent_id=12,name='snapshot.md',name_key=?,revision=revision+1 WHERE id=40", keyFor("snapshot.md")); err != nil {
				return err
			}
			again := check(t, txStore, txContext, 1, 11, keyFor("after.md"), 40)
			if !reflect.DeepEqual(first, again) {
				t.Fatal("同一快照混用了移动改名前后的记录")
			}
			check(t, txStore, txContext, 1, 12, keyFor("snapshot.md"), 0)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		check(t, store, ctx, 1, 11, keyFor("after.md"), 0)
		if current := check(t, store, ctx, 1, 12, keyFor("snapshot.md"), 40); current.Name != "snapshot.md" || current.Revision != 3 {
			t.Fatal("快照结束后未取得新父目录、名称及版本")
		}
	})

	// 6. 最大自增 ID 在普通夹具之后建立，核对整数/时间边界和连接故障。
	t.Run("full unsigned and database dates", func(t *testing.T) {
		maximum := ^uint64(0)
		minimumDate := time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
		maximumDate := time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'fixture_max','完整整数','fixture_hash')", maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,?,NULL,'',X'','DIR')", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.ExecContext(ctx, "INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size) VALUES(42,?,'MINIO','fixture-bucket','max-object',?)", maximum, maximum); err != nil {
			t.Fatal(err)
		}
		key := keyFor("最大值.bin")
		if _, err := pool.ExecContext(ctx, "INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size,revision,created_at,modified_at,updated_at) VALUES(?,?,?,'最大值.bin',?,'FILE','OTHER',42,?,?,?,?,?)", maximum-1, maximum, maximum, key, maximum, maximum, minimumDate, maximumDate, maximumDate); err != nil {
			t.Fatal(err)
		}
		entry := check(t, store, ctx, maximum, maximum, key, maximum-1)
		if entry.FileSize != maximum || entry.Revision != maximum || entry.CreatedAt != minimumDate || entry.ModifiedAt != maximumDate || entry.UpdatedAt != maximumDate {
			t.Fatal("完整无符号整数或数据库时间边界错误")
		}
	})
	t.Run("closed pool preserves database error", func(t *testing.T) {
		if err := pool.Close(); err != nil {
			t.Fatal(err)
		}
		if result, err := store.FindUserActiveFileEntryByName(ctx, 1, 11, keyFor("readme.md")); result != nil || err == nil || errors.Is(err, domain.ErrFileEntryNotFound) {
			t.Fatal("数据库连接故障被当作名称可用")
		}
	})
}
