package repository

import (
	"context"
	"errors"
	"math"
	"os/exec"
	"slices"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestAppendUserChangeEventIntegration(t *testing.T) {
	// 1. 复用独占库夹具，用正式迁移命令安装账号、目录和事件表。
	pool := schemaCheckDatabase(t)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	var databaseName string
	if err := pool.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&databaseName); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.CommandContext(ctx, "go", "run", "../../../cmd/database", "-database", databaseName).CombinedOutput(); err != nil {
		t.Fatalf("隔离库迁移失败: %v\n%s", err, output)
	}
	execute := func(statement string) {
		t.Helper()
		if _, err := pool.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	execute(`INSERT INTO users(id,username,display_name,password_hash,status,change_seq)
		VALUES(7,'event_fixture_a','Fixture A','fixture-hash','ACTIVE',0),
		      (8,'event_fixture_b','Fixture B','fixture-hash','ACTIVE',0),
		      (9,'event_fixture_c','Fixture C','fixture-hash','DISABLED',18446744073709551614)`)
	execute(`INSERT INTO user_devices(id,user_id,installation_id,name,platform)
		VALUES(11,7,'00000000-0000-4000-8000-000000000001','Fixture','WINDOWS')`)
	execute(`INSERT INTO auth_sessions(id,user_id,device_id,refresh_token_hash,expires_at)
		VALUES(17,7,11,UNHEX(REPEAT('17',32)),UTC_TIMESTAMP(6)+INTERVAL 1 HOUR)`)
	execute("INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(31,7,NULL,'',X'','DIR')")
	execute("INSERT INTO file_entries(id,user_id,parent_id,name,name_key,entry_type) VALUES(32,7,31,'fixture','fixture','DIR')")
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
	event := domain.ChangeEventInput{EntryID: 32, Revision: 2, EventType: "FIXTURE_CHANGE", Payload: []byte(`{"name":"合成目录"}`)}

	type accountState struct {
		sequence, events, revision uint64
	}
	readState := func(userID uint64) accountState {
		t.Helper()
		var state accountState
		if err := pool.QueryRowContext(ctx, "SELECT change_seq, (SELECT COUNT(*) FROM change_events WHERE user_id=users.id), "+
			"COALESCE((SELECT revision FROM file_entries WHERE user_id=users.id AND id=32),0) FROM users WHERE id=?", userID).
			Scan(&state.sequence, &state.events, &state.revision); err != nil {
			t.Fatal(err)
		}
		return state
	}
	inUserTransaction := func(input domain.ChangeEventInput, changeEntry bool, afterAppend func(context.Context, *Store, uint64, error) error) error {
		return store.WithUserTransaction(ctx, actor, func(txContext context.Context, txStore *Store) error {
			if changeEntry {
				if err := txStore.database.WithContext(txContext).Exec(
					"UPDATE file_entries SET revision=revision+1 WHERE id=32 AND user_id=?", actor.UserID).Error; err != nil {
					return err
				}
			}
			sequence, appendError := txStore.AppendUserChangeEvent(txContext, input)
			return afterAppend(txContext, txStore, sequence, appendError)
		})
	}

	// 2. 核对事务提交前后的外部可见性，以及账号和事件字段。
	if sequence, err := store.AppendUserChangeEvent(ctx, event); err == nil || sequence != 0 {
		t.Fatal("普通 Store 可越过账号事务追加事件", sequence, err)
	}
	invalid := event
	invalid.Revision = 0
	err = inUserTransaction(invalid, false, func(_ context.Context, _ *Store, sequence uint64, appendError error) error {
		if sequence != 0 {
			t.Error("无效事件分配了序号", sequence)
		}
		return appendError
	})
	if !errors.Is(err, domain.ErrInvalidChangeEventInput) || readState(7) != (accountState{revision: 1}) {
		t.Fatal("无效事件变更了账号或业务数据", err)
	}
	err = inUserTransaction(event, true, func(_ context.Context, _ *Store, sequence uint64, appendError error) error {
		if appendError != nil {
			return appendError
		}
		if sequence != 1 || readState(7) != (accountState{revision: 1}) {
			t.Error("提交前其他连接已看到事件、序号或业务版本")
		}
		return nil
	})
	if err != nil || readState(7) != (accountState{sequence: 1, events: 1, revision: 2}) {
		t.Fatal("提交后未同时读到账号序号、事件与业务版本", err)
	}
	var entryID, revision uint64
	var eventType, name string
	var createdAt time.Time
	if err := pool.QueryRowContext(ctx, "SELECT entry_id,revision,event_type,payload->>'$.name',created_at "+
		"FROM change_events WHERE user_id=7 AND `sequence`=1").Scan(&entryID, &revision, &eventType, &name, &createdAt); err != nil ||
		entryID != 32 || revision != 2 || eventType != "FIXTURE_CHANGE" || name != "合成目录" ||
		createdAt.IsZero() || createdAt.Location() != time.UTC || createdAt.Nanosecond()%1000 != 0 {
		t.Fatal("正式事件列、JSON 内容或数据库 UTC 时间不符", err)
	}

	// 3. 注入真实事件 INSERT 失败，并验证主动回滚和重试也复用同一个序号。
	execute(`CREATE TRIGGER fixture_reject_event BEFORE INSERT ON change_events FOR EACH ROW
		SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='fixture event insert rejected'`)
	event.Revision = 3
	err = inUserTransaction(event, true, func(txContext context.Context, txStore *Store, sequence uint64, appendError error) error {
		var mysqlError *mysql.MySQLError
		if sequence != 0 || !errors.As(appendError, &mysqlError) || mysqlError.Number != 1644 {
			t.Error("未收到预期的事件 INSERT 错误", sequence, appendError)
		}
		var advanced uint64
		readError := txStore.database.WithContext(txContext).Raw("SELECT change_seq FROM users WHERE id=7").Row().Scan(&advanced)
		if readError != nil || advanced != 2 {
			t.Error("未在回滚前经过真实的序号推进", advanced, readError)
		}
		return appendError
	})
	beforeRetry := accountState{sequence: 1, events: 1, revision: 2}
	if err == nil || readState(7) != beforeRetry {
		t.Fatal("事件写入失败后业务版本、序号或事件数发生变化", err)
	}
	execute("DROP TRIGGER fixture_reject_event")
	stopAfterBothWrites := errors.New("fixture rolls back the completed callback")
	err = inUserTransaction(event, true, func(_ context.Context, _ *Store, sequence uint64, appendError error) error {
		if appendError != nil {
			return appendError
		}
		if sequence != 2 {
			t.Error("写入失败后未重新使用原候选序号", sequence)
		}
		return stopAfterBothWrites
	})
	if !errors.Is(err, stopAfterBothWrites) || readState(7) != beforeRetry {
		t.Fatal("外层回滚后业务版本、序号或事件数发生变化", err)
	}
	err = inUserTransaction(event, true, func(_ context.Context, _ *Store, sequence uint64, appendError error) error {
		if appendError != nil {
			return appendError
		}
		if sequence != 2 {
			t.Error("外层回滚后出现同步序号断档", sequence)
		}
		return nil
	})
	if err != nil || readState(7) != (accountState{sequence: 2, events: 2, revision: 3}) {
		t.Fatal("回滚后的成功重试事实不符", err)
	}
	t.Log("账号 7 已提交 1，再经历 INSERT 失败和主动回滚，重试仍使用 2，业务版本与两次回滚一致")

	// 4. 同账号并发取得连续序号，另一账号独立起步，满 uint64 时保留原事实。
	appendForUser := func(userID uint64, input domain.ChangeEventInput) (uint64, error) {
		var sequence uint64
		const allowDisabled = true
		err := store.withLockedUserTransaction(ctx, userID, allowDisabled, func(txContext context.Context, txStore *Store) error {
			var appendError error
			sequence, appendError = txStore.AppendUserChangeEvent(txContext, input)
			return appendError
		})
		return sequence, err
	}
	type appendResult struct {
		sequence uint64
		err      error
	}
	const concurrentCount = 4
	start := make(chan struct{})
	results := make(chan appendResult, concurrentCount)
	for range concurrentCount {
		go func() {
			<-start
			sequence, err := appendForUser(7, event)
			results <- appendResult{sequence: sequence, err: err}
		}()
	}
	close(start)
	var sequences []uint64
	for range concurrentCount {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		sequences = append(sequences, result.sequence)
	}
	slices.Sort(sequences)
	for index, sequence := range sequences {
		if sequence != uint64(index+3) {
			t.Fatal("并发追加得到不连续的序号", sequences)
		}
	}
	if readState(7) != (accountState{sequence: 6, events: 6, revision: 3}) {
		t.Fatal("并发提交的账号和事件账目不符")
	}
	// 合成墓碑标识可以没有当前目录项，事件所属账号仍取自事务。
	event.EntryID = 99
	if sequence, err := appendForUser(8, event); err != nil || sequence != 1 ||
		readState(8) != (accountState{sequence: 1, events: 1}) {
		t.Fatal("另一账号的序号不是独立起步", sequence, err)
	}
	if sequence, err := appendForUser(9, event); err != nil || sequence != math.MaxUint64 {
		t.Fatal("后台用户事务无法保存最大序号", sequence, err)
	}
	if sequence, err := appendForUser(9, event); sequence != 0 || !errors.Is(err, domain.ErrChangeSequenceExhausted) ||
		readState(9) != (accountState{sequence: math.MaxUint64, events: 1}) {
		t.Fatal("序号达到上限后的处理异常", sequence, err)
	}
	t.Logf("并发序号 %v；另一账号从 1 开始；uint64 上限可保存一次，后续追加被拒绝", sequences)
}
