package repository

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestFindUserVisibleReusableObjectIntegration(t *testing.T) {
	pool := userProfileDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	execute := func(statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	addDirectory := func(id, parentID uint64, name string) {
		t.Helper()
		execute("INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(?,7,?,?,?,'DIR')", id, parentID, name, []byte(name))
	}
	const ownUser, otherUser, ownObject, otherObject, root, file = 7, 8, 23, 24, 13, 31
	content := []byte("fixture")
	size := uint64(len(content))
	digest := sha256.Sum256(content)
	emptyDigest := sha256.Sum256(nil)

	// 1. 准备同摘要的两个账号、两级父链和一个零字节 READY 对象。
	execute(`INSERT INTO users(id,username,display_name,password_hash) VALUES
		(7,'reuse_fixture','复用夹具','fixture_hash'),(8,'reuse_other','其他夹具','fixture_hash')`)
	execute("INSERT INTO file_entries(id,user_id,name,name_key,entry_type) VALUES(13,7,'',X'','DIR'),(14,8,'',X'','DIR')")
	execute(`INSERT INTO stored_objects(id,owner_user_id,provider,bucket,object_key,file_size,verified_sha256,verified_at,status) VALUES
		(23,7,'local_minio','fixture-bucket','own-ready',?,?,UTC_TIMESTAMP(6),'READY'),
		(24,8,'local_minio','fixture-bucket','other-ready',?,?,UTC_TIMESTAMP(6),'READY'),
		(25,7,'local_minio','fixture-bucket','empty-ready',0,?,UTC_TIMESTAMP(6),'READY')`, size, digest[:], size, digest[:], emptyDigest[:])
	addDirectory(17, root, "folder")
	addDirectory(18, 17, "child")
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size) VALUES
		(31,7,18,'nested.bin',X'6E6573746564','FILE','OTHER',23,?),
		(32,8,14,'other.bin',X'6F74686572','FILE','OTHER',24,?),
		(33,7,13,'empty.bin',X'656D707479','FILE','OTHER',25,0)`, size, size)
	check := func(userID uint64, contentSHA [sha256.Size]byte, bytes uint64, wantObjectID uint64) {
		t.Helper()
		object, err := store.FindUserVisibleReusableObject(ctx, userID, contentSHA, bytes)
		if wantObjectID == 0 {
			if err != nil || object != nil {
				t.Fatal("不可复用对象未统一返回未命中", object, err)
			}
			return
		}
		if err != nil || object == nil || object.ID != wantObjectID || object.OwnerUserID != userID || object.FileSize != bytes ||
			object.VerifiedSHA256 == nil || *object.VerifiedSHA256 != contentSHA {
			t.Fatal("可复用对象的归属、大小或摘要不符", object, err)
		}
	}
	check(ownUser, digest, size, ownObject)
	check(otherUser, digest, size, otherObject)
	check(ownUser, emptyDigest, 0, 25)
	check(ownUser, digest, size+1, 0)
	check(ownUser, sha256.Sum256([]byte("different")), size, 0)

	// 2. 只认可当前账号下的活动文件和完整活动目录链。
	execute("UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6) WHERE id=?", file)
	check(ownUser, digest, size, 0)
	execute("UPDATE file_entries SET status='ACTIVE',deleted_at=NULL WHERE id=?", file)
	execute("UPDATE file_entries SET status='TRASHED',deleted_at=UTC_TIMESTAMP(6) WHERE id=18")
	check(ownUser, digest, size, 0)
	execute("UPDATE file_entries SET status='ACTIVE',deleted_at=NULL WHERE id=18")
	execute("UPDATE file_entries SET status='PURGING',purge_started_at=UTC_TIMESTAMP(6) WHERE id=17")
	check(ownUser, digest, size, 0)
	check(otherUser, digest, size, otherObject)
	execute("UPDATE file_entries SET status='ACTIVE',purge_started_at=NULL WHERE id=17")
	execute("UPDATE users SET status='DISABLED' WHERE id=?", ownUser)
	check(ownUser, digest, size, 0)
	execute("UPDATE users SET status='ACTIVE' WHERE id=?", ownUser)
	execute("UPDATE stored_objects SET status='DELETING' WHERE id=?", ownObject)
	check(ownUser, digest, size, 0)
	execute("UPDATE stored_objects SET status='READY' WHERE id=?", ownObject)

	// 3. 拒绝目录循环和超深路径，合法深度与并存的可见引用仍可命中。
	execute("UPDATE file_entries SET parent_id=18 WHERE id=17")
	check(ownUser, digest, size, 0)
	var parentID uint64 = root
	for depth := 1; depth <= domain.MaxFileEntryDepth; depth++ {
		dirID := uint64(100 + depth)
		addDirectory(dirID, parentID, fmt.Sprintf("depth-%d", depth))
		parentID = dirID
	}
	execute("UPDATE file_entries SET parent_id=? WHERE id=?", parentID, file)
	check(ownUser, digest, size, 0)
	execute("UPDATE file_entries SET parent_id=? WHERE id=?", parentID-1, file)
	check(ownUser, digest, size, ownObject)
	execute("UPDATE file_entries SET parent_id=18 WHERE id=?", file)
	execute(`INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type,category,object_id,file_size)
		VALUES(34,7,13,'visible.bin',X'76697369626C65','FILE','OTHER',23,?)`, size)
	before, err := store.FindUserStoredObject(ctx, ownUser, ownObject)
	if err != nil {
		t.Fatal(err)
	}
	check(ownUser, digest, size, ownObject)
	after, err := store.FindUserStoredObject(ctx, ownUser, ownObject)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("只读查询改变了对象记录", err)
	}
	stopped, stop := context.WithCancel(ctx)
	stop()
	if object, err := store.FindUserVisibleReusableObject(stopped, ownUser, digest, size); object != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("取消时未返回停止原因", object, err)
	}
	t.Log("匹配同账号、摘要和大小，回收路径、循环和超深路径均未命中")
}
