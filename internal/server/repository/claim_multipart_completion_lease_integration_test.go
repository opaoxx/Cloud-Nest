package repository

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestClaimUserMultipartCompletionLeaseIntegration(t *testing.T) {
	// 1. 在独占测试库安装正式任务和会话结构，构造一条两片的合成任务。
	pool, _ := uploadTaskDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	execute := func(t *testing.T, statement string, args ...any) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	start := strings.Index(migrations.InitialSQL, "CREATE TABLE auth_sessions (")
	end := strings.Index(migrations.InitialSQL, "CREATE TABLE stored_objects (")
	if start < 0 || end <= start {
		t.Fatal("未找到认证会话表定义")
	}
	execute(t, migrations.InitialSQL[start:end])
	execute(t, migrations.Ordered()[1].SQL)
	execute(t, "INSERT INTO user_devices(id,user_id,installation_id,name,platform) VALUES(15,7,'58ee758a-0727-42de-ae7d-3bb33155e148','second-device','WEB')")
	execute(t, "UPDATE users SET status='ACTIVE' WHERE id=8")
	execute(t, `INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at) VALUES
		(17,7,11,UNHEX(REPEAT('11',32)),UTC_TIMESTAMP(6)+INTERVAL 1 DAY),
		(18,7,15,UNHEX(REPEAT('22',32)),UTC_TIMESTAMP(6)+INTERVAL 1 DAY),
		(22,8,12,UNHEX(REPEAT('33',32)),UTC_TIMESTAMP(6)+INTERVAL 1 DAY)`)
	const fileSize, userID, taskID = 16777217, 7, 19
	execute(t, "UPDATE users SET reserved_storage=? WHERE id=7", fileSize)
	execute(t, "UPDATE stored_objects SET file_size=? WHERE id=23", fileSize)
	primary := domain.Actor{UserID: userID, DeviceID: 11, SessionID: 17}
	secondary := domain.Actor{UserID: userID, DeviceID: 15, SessionID: 18}
	foreign := domain.Actor{UserID: 8, DeviceID: 12, SessionID: 22}
	reset := func(t *testing.T) {
		t.Helper()
		execute(t, `UPDATE upload_tasks SET upload_mode='MULTIPART',status='UPLOADING',storage_upload_id='fixture+A/=',
			file_size=?,part_size=16777216,part_count=2,expected_sha256=UNHEX(REPEAT('ab',32)),reserved_bytes=?,
			published_object_id=NULL,resolved_name=NULL,result_entry_id=NULL,completed_at=NULL,
			lease_owner=NULL,lease_until=NULL,lease_version=4,updated_at=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND,
			expires_at=UTC_TIMESTAMP(6)+INTERVAL 1 HOUR WHERE user_id=7 AND id=19`, fileSize, fileSize)
	}
	read := func(t *testing.T) *domain.UploadTask {
		t.Helper()
		task, err := store.FindUserUploadTask(ctx, userID, taskID)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	claim := func(actor domain.Actor, expected *domain.UploadTask, owner string) (*domain.UploadTask, error) {
		return store.ClaimUserMultipartCompletionLease(ctx, actor, expected, owner, service.ResolveMultipartUploadCompletionStatus)
	}

	t.Run("two devices and one winner", func(t *testing.T) {
		// 2. 同账号两台设备基于同一快照并发领取，只有一份租约可以提交。
		reset(t)
		before := read(t)
		type outcome struct {
			task *domain.UploadTask
			err  error
		}
		startTogether := make(chan struct{})
		outcomes := make(chan outcome, 2)
		for index, actor := range []domain.Actor{primary, secondary} {
			go func() {
				<-startTogether
				task, err := claim(actor, before, []string{"completion-first", "completion-second"}[index])
				outcomes <- outcome{task: task, err: err}
			}()
		}
		close(startTogether)
		var winner *domain.UploadTask
		busy := 0
		for range 2 {
			result := <-outcomes
			switch {
			case result.err == nil && result.task != nil && winner == nil:
				winner = result.task
			case result.task == nil && errors.Is(result.err, domain.ErrUploadTaskBusy):
				busy++
			default:
				t.Fatal("领取结果异常", result.err)
			}
		}
		if winner == nil || busy != 1 || winner.LeaseVersion != 5 || winner.LeaseOwner == nil ||
			winner.LeaseUntil == nil || !winner.LeaseUntil.Equal(winner.UpdatedAt.Add(time.Minute)) ||
			!winner.UpdatedAt.After(before.UpdatedAt) || !winner.UpdatedAt.Before(before.ExpiresAt) {
			t.Fatal("并发领取未产生唯一的六十秒租约")
		}
		before.LeaseOwner, before.LeaseUntil, before.LeaseVersion, before.UpdatedAt = winner.LeaseOwner, winner.LeaseUntil, winner.LeaseVersion, winner.UpdatedAt
		if !reflect.DeepEqual(before, winner) || !reflect.DeepEqual(winner, read(t)) {
			t.Fatal("领取修改了租约和更新时间以外的任务字段")
		}
		if err := service.ValidateUploadPartSigningState(winner, userID, taskID, winner.UpdatedAt); !errors.Is(err, domain.ErrUploadTaskBusy) {
			t.Fatal("已提交的完成核对租约没有阻止分片签发", err)
		}
		var used, reserved, parts uint64
		err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage,(SELECT COUNT(*) FROM upload_parts) FROM users WHERE id=7").
			Scan(&used, &reserved, &parts)
		if err != nil || used != 0 || reserved != fileSize || parts != 0 {
			t.Fatal("仅领取租约时应保留配额和空完成快照", used, reserved, parts, err)
		}
		t.Logf("并发结果：1 个领取、1 个忙，租约版本 %d，预占 %d 字节，完成快照 %d 条", winner.LeaseVersion, reserved, parts)
	})

	t.Run("expired lease can be replaced", func(t *testing.T) {
		reset(t)
		execute(t, "UPDATE upload_tasks SET lease_owner='old-worker',lease_until=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE id=19")
		result, err := claim(secondary, read(t), "completion-new")
		if err != nil || result == nil || result.LeaseOwner == nil || *result.LeaseOwner != "completion-new" ||
			result.LeaseVersion != 5 || !reflect.DeepEqual(result, read(t)) {
			t.Fatal("未能在旧租约到期后重新领取", err)
		}
	})

	// 3. 拒绝过期任务、旧对象会话和版本，并确认每次失败后都没有写入。
	for _, test := range []struct {
		name, change string
		want         error
	}{
		{"expired task", "expires_at=UTC_TIMESTAMP(6)", domain.ErrUploadTaskNotReady},
		{"already accepted", "status='COMPLETING'", domain.ErrUploadTaskNotReady},
		{"session changed only in case", "storage_upload_id='Fixture+A/='", domain.ErrUploadTaskBusy},
		{"version changed", "lease_version=5", domain.ErrUploadTaskBusy},
	} {
		t.Run(test.name, func(t *testing.T) {
			reset(t)
			expected := read(t)
			execute(t, "UPDATE upload_tasks SET "+test.change+" WHERE id=19")
			before := read(t)
			if result, err := claim(primary, expected, "completion-stale"); result != nil || !errors.Is(err, test.want) {
				t.Fatal("未按预期拒绝过期或失配的快照", err)
			}
			if !reflect.DeepEqual(before, read(t)) {
				t.Fatal("领取失败改变了任务")
			}
		})
	}
	t.Run("ownership and lease version ceiling", func(t *testing.T) {
		reset(t)
		execute(t, "UPDATE upload_tasks SET lease_version=? WHERE id=19", ^uint64(0))
		before := read(t)
		forged := *before
		forged.UserID = foreign.UserID
		if result, err := claim(foreign, &forged, "completion-foreign"); result != nil || !errors.Is(err, domain.ErrUploadTaskNotFound) {
			t.Fatal("任务锁未限制在当前认证账号", err)
		}
		if result, err := claim(primary, before, "completion-next"); result != nil || !errors.Is(err, domain.ErrUploadLeaseVersionExhausted) {
			t.Fatal("版本达到上限后仍允许领取", err)
		}
		if !reflect.DeepEqual(before, read(t)) {
			t.Fatal("归属或版本拒绝改变了任务")
		}
	})

	t.Run("release", func(t *testing.T) {
		claimForRelease := func(t *testing.T) *domain.UploadTask {
			t.Helper()
			reset(t)
			claimed, err := claim(primary, read(t), "completion-owner")
			if err != nil || claimed == nil {
				t.Fatal("测试租约领取失败", err)
			}
			return claimed
		}
		assertRelease := func(t *testing.T, actor domain.Actor, claimed *domain.UploadTask, want bool, wantError error) {
			t.Helper()
			released, err := store.ReleaseUserMultipartCompletionLease(ctx, actor, claimed)
			if released != want || !errors.Is(err, wantError) {
				t.Fatalf("释放结果错误，得到 %t、%v，预期 %t、%v", released, err, want, wantError)
			}
		}

		// 1. 真实领取后释放，确认恢复签发资格并且重复释放不再写入。
		t.Run("current lease only releases once", func(t *testing.T) {
			claimed := claimForRelease(t)
			assertRelease(t, primary, claimed, true, nil)
			released := read(t)
			expected := *claimed
			expected.LeaseOwner, expected.LeaseUntil, expected.UpdatedAt = nil, nil, released.UpdatedAt
			if released.UpdatedAt.Before(claimed.UpdatedAt) || !reflect.DeepEqual(&expected, released) {
				t.Fatal("释放修改了持有者、租约截止时间和更新时间以外的字段")
			}
			if err := service.ValidateUploadPartSigningState(released, userID, taskID, released.UpdatedAt); err != nil {
				t.Fatal("释放后未恢复分片签发资格", err)
			}
			assertRelease(t, primary, claimed, false, nil)
			if !reflect.DeepEqual(released, read(t)) {
				t.Fatal("重复释放改变了任务")
			}
			var used, reserved, parts uint64
			err := pool.QueryRowContext(ctx, "SELECT used_storage,reserved_storage,(SELECT COUNT(*) FROM upload_parts) FROM users WHERE id=7").
				Scan(&used, &reserved, &parts)
			if err != nil || used != 0 || reserved != fileSize || parts != 0 {
				t.Fatal("释放改变了配额或完成快照", used, reserved, parts, err)
			}
		})

		// 2. 旧租约到期后重新领取，即使持有者标识相同，旧版本也不能释放新租约。
		t.Run("late release preserves replacement", func(t *testing.T) {
			old := claimForRelease(t)
			execute(t, "UPDATE upload_tasks SET lease_until=UTC_TIMESTAMP(6)-INTERVAL 1 SECOND WHERE id=19")
			current, err := claim(secondary, read(t), *old.LeaseOwner)
			if err != nil || current == nil || current.LeaseVersion != old.LeaseVersion+1 {
				t.Fatal("新版本领取失败", err)
			}
			assertRelease(t, primary, old, false, nil)
			if !reflect.DeepEqual(current, read(t)) {
				t.Fatal("旧版本释放改变了新租约")
			}
			assertRelease(t, secondary, current, true, nil)
			t.Logf("旧版本 %d 未改动新版本 %d，新请求成功释放", old.LeaseVersion, current.LeaseVersion)
		})

		// 3. 会话、持有者、账号或阶段不匹配时，目标保持原样。
		t.Run("identity and stage constraints", func(t *testing.T) {
			claimed := claimForRelease(t)
			wrongOwner := *claimed
			wrongOwner.LeaseOwner = new("Completion-owner")
			wrongSession := *claimed
			wrongSession.StorageUploadID = new("Fixture+A/=")
			assertRelease(t, primary, &wrongOwner, false, nil)
			assertRelease(t, primary, &wrongSession, false, nil)
			assertRelease(t, foreign, claimed, false, domain.ErrUploadTaskNotFound)
			foreignScope := *claimed
			foreignScope.UserID = foreign.UserID
			assertRelease(t, foreign, &foreignScope, false, nil)
			if !reflect.DeepEqual(claimed, read(t)) {
				t.Fatal("绑定或认证账号不匹配时改变了任务")
			}
			execute(t, "UPDATE upload_tasks SET status='COMPLETING' WHERE id=19")
			before := read(t)
			assertRelease(t, primary, claimed, false, nil)
			if !reflect.DeepEqual(before, read(t)) {
				t.Fatal("进入 COMPLETING 后的租约被旧请求修改")
			}
		})
		t.Run("revoked session", func(t *testing.T) {
			claimed := claimForRelease(t)
			execute(t, "UPDATE auth_sessions SET revoked_at=UTC_TIMESTAMP(6) WHERE id=17")
			assertRelease(t, primary, claimed, false, domain.ErrSessionRevoked)
			if !reflect.DeepEqual(claimed, read(t)) {
				t.Fatal("撤销后的会话改变了任务")
			}
		})
	})

	var databaseName string
	if err := pool.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&databaseName); err != nil {
		t.Fatal(err)
	}
	t.Logf("独占验证库 %s", databaseName)
}
