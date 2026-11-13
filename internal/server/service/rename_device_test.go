package service

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

type renameDeviceTxStub struct {
	rename func(context.Context, uint64, string) (*domain.UserDevice, error)
}

func (transaction *renameDeviceTxStub) RenameUserDevice(ctx context.Context, targetID uint64, name string) (*domain.UserDevice, error) {
	return transaction.rename(ctx, targetID, name)
}

func renameDeviceServiceFixture(actor domain.Actor, targetID uint64, name string) *domain.UserDevice {
	return &domain.UserDevice{ID: targetID, UserID: actor.UserID, Name: name, Platform: "WEB", CreatedAt: time.Date(2026, 11, 13, 0, 0, 0, 123456000, time.UTC)}
}

func TestRenameDeviceServiceCommit(t *testing.T) {
	for _, testCase := range []struct {
		label, name         string
		targetID            uint64
		maximum, historical bool
	}{
		{label: "another device", targetID: 19, name: "学习电脑"},
		{label: "current device", targetID: 11, name: "当前电脑"},
		{label: "full unsigned ids", targetID: ^uint64(0), name: "完整 ID", maximum: true},
		{label: "padded name", targetID: 19, name: "  Study PC 🎮  "},
		{label: "maximum emoji", targetID: 19, name: strings.Repeat("🎮", 100)},
		{label: "historical revoked target", targetID: 19, name: "历史设备", historical: true},
	} {
		t.Run(testCase.label, func(t *testing.T) {
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			if testCase.maximum {
				actor = domain.Actor{UserID: ^uint64(0), DeviceID: ^uint64(0) - 1, SessionID: ^uint64(0) - 2}
			}
			source := renameDeviceServiceFixture(actor, testCase.targetID, testCase.name)
			if testCase.historical {
				changedAt := source.CreatedAt.Add(time.Minute)
				source.LastSeenAt, source.RevokedAt = &changedAt, &changedAt
			}
			original := *source
			type transactionKey struct{}
			ctx := context.Background()
			txContext := context.WithValue(ctx, transactionKey{}, true)
			var steps []string
			rename := NewRenameDeviceService(func(received context.Context, identity domain.Actor, action func(context.Context, RenameDeviceTransaction) error) error {
				steps = append(steps, "begin")
				if received != ctx || identity != actor {
					t.Error("事务上下文或认证身份错误")
				}
				txStore := &renameDeviceTxStub{rename: func(received context.Context, targetID uint64, name string) (*domain.UserDevice, error) {
					steps = append(steps, "rename")
					if received != txContext || targetID != testCase.targetID || name != testCase.name {
						t.Error("事务方法未收到准确参数或原名称")
					}
					return source, nil
				}}
				if err := action(txContext, txStore); err != nil {
					return err
				}
				steps = append(steps, "commit")
				return nil
			})
			result, err := rename.Rename(ctx, actor, testCase.targetID, testCase.name)
			if err != nil || result == nil || !reflect.DeepEqual(*result, original) || !reflect.DeepEqual(steps, []string{"begin", "rename", "commit"}) {
				t.Fatal("提交结果或顺序错误", err)
			}
			if result == source || (source.LastSeenAt != nil && (result.LastSeenAt == source.LastSeenAt || result.RevokedAt == source.RevokedAt || result.LastSeenAt == result.RevokedAt)) {
				t.Fatal("服务结果或可空时间共享了来源对象")
			}
			result.Name = "changed"
			if result.LastSeenAt != nil {
				*result.LastSeenAt = result.LastSeenAt.Add(time.Hour)
			}
			if !reflect.DeepEqual(*source, original) {
				t.Fatal("修改返回值影响了来源设备")
			}
			second, err := rename.Rename(ctx, actor, testCase.targetID, testCase.name)
			if err != nil || second == nil || second == result || !reflect.DeepEqual(*second, original) {
				t.Fatal("重复调用结果不独立", err)
			}
		})
	}
}

func TestRenameDeviceServiceFailures(t *testing.T) {
	fault := errors.New("fixture transaction fault")
	for _, testCase := range []struct {
		label                                         string
		entryError, renameError, commitError, want    error
		skipCallback, nilContext, nilStore, nilDevice bool
		change                                        func(*domain.UserDevice)
		calls                                         int
	}{
		{label: "begin fault", entryError: fault, want: fault},
		{label: "caller missing session", entryError: domain.ErrSessionNotFound, want: domain.ErrSessionNotFound},
		{label: "caller missing device", entryError: domain.ErrDeviceNotFound, want: domain.ErrDeviceNotFound},
		{label: "caller revoked session", entryError: domain.ErrSessionRevoked, want: domain.ErrSessionRevoked},
		{label: "caller expired session", entryError: domain.ErrSessionExpired, want: domain.ErrSessionExpired},
		{label: "caller revoked device", entryError: domain.ErrDeviceRevoked, want: domain.ErrDeviceRevoked},
		{label: "missing account", entryError: domain.ErrAccountNotFound, want: domain.ErrAccountNotFound},
		{label: "disabled account", entryError: domain.ErrAccountDisabled, want: domain.ErrAccountDisabled},
		{label: "invalid caller", entryError: domain.ErrInvalidSessionData, want: domain.ErrInvalidSessionData},
		{label: "callback omitted", skipCallback: true, want: domain.ErrInvalidDeviceData},
		{label: "nil callback context", nilContext: true},
		{label: "nil callback store", nilStore: true},
		{label: "missing target", renameError: domain.ErrDeviceNotFound, want: domain.ErrTargetDeviceNotFound, calls: 1},
		{label: "wrapped missing target", renameError: fmt.Errorf("target: %w", domain.ErrDeviceNotFound), want: domain.ErrTargetDeviceNotFound, calls: 1},
		{label: "existing target classification", renameError: domain.ErrTargetDeviceNotFound, want: domain.ErrTargetDeviceNotFound, calls: 1},
		{label: "write fault", renameError: fault, want: fault, calls: 1},
		{label: "nil device", nilDevice: true, want: domain.ErrInvalidDeviceData, calls: 1},
		{label: "wrong target", change: func(device *domain.UserDevice) { device.ID = 99 }, want: domain.ErrInvalidDeviceData, calls: 1},
		{label: "wrong owner", change: func(device *domain.UserDevice) { device.UserID = 99 }, want: domain.ErrInvalidDeviceData, calls: 1},
		{label: "wrong name", change: func(device *domain.UserDevice) { device.Name = "other name" }, want: domain.ErrInvalidDeviceData, calls: 1},
		{label: "zero creation time", change: func(device *domain.UserDevice) { device.CreatedAt = time.Time{} }, want: domain.ErrInvalidDeviceData, calls: 1},
		{label: "commit fault", commitError: fault, want: fault, calls: 1},
	} {
		t.Run(testCase.label, func(t *testing.T) {
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			calls := 0
			rename := NewRenameDeviceService(func(ctx context.Context, _ domain.Actor, action func(context.Context, RenameDeviceTransaction) error) error {
				if testCase.entryError != nil || testCase.skipCallback {
					return testCase.entryError
				}
				txStore := &renameDeviceTxStub{rename: func(context.Context, uint64, string) (*domain.UserDevice, error) {
					calls++
					source := renameDeviceServiceFixture(actor, 19, "新名称")
					if testCase.change != nil {
						testCase.change(source)
					}
					if testCase.nilDevice {
						source = nil
					}
					return source, testCase.renameError
				}}
				if testCase.nilContext {
					ctx = nil
				}
				var transaction RenameDeviceTransaction = txStore
				if testCase.nilStore {
					transaction = nil
				}
				if err := action(ctx, transaction); err != nil {
					return err
				}
				return testCase.commitError
			})
			result, err := rename.Rename(context.Background(), actor, 19, "新名称")
			if result != nil || err == nil || (testCase.want != nil && !errors.Is(err, testCase.want)) || calls != testCase.calls {
				t.Fatal("失败结果、原因或调用次数错误", err, calls)
			}
			if errors.Is(err, domain.ErrTargetDeviceNotFound) != (testCase.want == domain.ErrTargetDeviceNotFound) {
				t.Fatal("调用者与目标错误混淆")
			}
			if testCase.want == domain.ErrTargetDeviceNotFound && errors.Is(err, domain.ErrDeviceNotFound) {
				t.Fatal("目标错误仍带有原设备错误分类")
			}
		})
	}
}

func TestRenameDeviceServiceInput(t *testing.T) {
	for _, label := range []string{"nil context", "zero user", "zero device", "zero session", "zero target", "empty name", "whitespace name", "control name", "long name", "invalid utf8", "cancelled", "expired"} {
		t.Run(label, func(t *testing.T) {
			actor := domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}
			targetID, name := uint64(19), "新名称"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var callContext context.Context = ctx
			switch label {
			case "nil context":
				callContext = nil
			case "zero user":
				actor.UserID = 0
			case "zero device":
				actor.DeviceID = 0
			case "zero session":
				actor.SessionID = 0
			case "zero target":
				targetID = 0
			case "empty name":
				name = ""
			case "whitespace name":
				name = " \u3000 "
			case "control name":
				name = "设备\n"
			case "long name":
				name = strings.Repeat("机", 101)
			case "invalid utf8":
				name = "设备\xff"
			case "cancelled":
				cancel()
			case "expired":
				expired, stop := context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer stop()
				callContext = expired
			}
			called := false
			rename := NewRenameDeviceService(func(context.Context, domain.Actor, func(context.Context, RenameDeviceTransaction) error) error {
				called = true
				return nil
			})
			result, err := rename.Rename(callContext, actor, targetID, name)
			if result != nil || err == nil || called {
				t.Fatal("非法输入进入事务")
			}
			if label == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal("取消原因丢失")
			}
			if label == "expired" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("期限原因丢失")
			}
		})
	}
}

func TestRenameDeviceServiceDependency(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("缺少事务依赖仍构造成功")
		}
	}()
	NewRenameDeviceService(nil)
}

func TestRenameDeviceServiceContexts(t *testing.T) {
	for _, stage := range []string{"before rename", "after rename", "cancel before missing target classification", "rename deadline", "after confirmed commit", "cancelled commit failure"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			txDuration := time.Second
			if stage == "rename deadline" {
				txDuration = 20 * time.Millisecond
			}
			txContext, cancelTransaction := context.WithTimeout(ctx, txDuration)
			defer cancelTransaction()
			failure := errors.New("fixture commit fault")
			calls := 0
			rename := NewRenameDeviceService(func(ctx context.Context, actor domain.Actor, action func(context.Context, RenameDeviceTransaction) error) error {
				if stage == "before rename" {
					cancelTransaction()
				}
				transaction := &renameDeviceTxStub{rename: func(received context.Context, targetID uint64, name string) (*domain.UserDevice, error) {
					calls++
					if received != txContext {
						t.Error("改名上下文被替换")
					}
					switch stage {
					case "rename deadline":
						<-received.Done()
						return nil, received.Err()
					case "after rename":
						cancelTransaction()
					case "cancel before missing target classification":
						cancelTransaction()
						return nil, domain.ErrDeviceNotFound
					}
					return renameDeviceServiceFixture(actor, targetID, name), nil
				}}
				if err := action(txContext, transaction); err != nil {
					return err
				}
				cancel()
				if stage == "cancelled commit failure" {
					return failure
				}
				return nil
			})
			result, err := rename.Rename(ctx, domain.Actor{UserID: 7, DeviceID: 11, SessionID: 17}, 19, "新名称")
			switch stage {
			case "after confirmed commit":
				if err != nil || result == nil || result.Name != "新名称" || ctx.Err() == nil {
					t.Fatal("已确认提交结果丢失", err)
				}
			case "cancelled commit failure":
				if result != nil || !errors.Is(err, failure) {
					t.Fatal("提交故障被取消覆盖", err)
				}
			default:
				want := error(context.Canceled)
				if stage == "rename deadline" {
					want = context.DeadlineExceeded
				}
				if result != nil || !errors.Is(err, want) || errors.Is(err, domain.ErrTargetDeviceNotFound) {
					t.Fatal("事务取消原因或目标分类错误", err)
				}
			}
			if stage == "before rename" && calls != 0 {
				t.Error("事务取消后进入改名")
			}
		})
	}
}
