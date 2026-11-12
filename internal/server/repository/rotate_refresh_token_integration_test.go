package repository

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestRotateRefreshTokenIntegration(t *testing.T) {
	pool := loginSessionDatabase(t)
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	verifier := service.NewRefreshCredentialService(store.FindRefreshSession)
	type fixture struct {
		raw      string
		verified domain.VerifiedRefreshSession
	}
	createFixture := func(t *testing.T, userID uint64) fixture {
		t.Helper()
		raw, hash := service.GenerateRefreshToken()
		err := store.WithLoginUserTransaction(ctx, userID, func(txContext context.Context, txStore *Store) error {
			device, err := txStore.EnsureLoginDevice(txContext, uuid.NewString(), "摘要轮换测试设备", "WEB")
			if err != nil {
				return err
			}
			_, err = txStore.CreateLoginSession(txContext, device.ID, hash, time.Now().Add(time.Hour))
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		verified, err := verifier.Verify(ctx, raw)
		if err != nil || verified == nil {
			t.Fatal("建立真实刷新夹具失败", err)
		}
		return fixture{raw: raw, verified: *verified}
	}
	assertCurrent := func(t *testing.T, raw string, want domain.VerifiedRefreshSession) {
		t.Helper()
		current, err := verifier.Verify(ctx, raw)
		if err != nil || current == nil || *current != want {
			t.Fatal("有效原文未定位到预期会话和代次", err)
		}
	}
	assertInvalid := func(t *testing.T, raw string) {
		t.Helper()
		if current, err := verifier.Verify(ctx, raw); current != nil || !errors.Is(err, domain.ErrInvalidRefreshToken) {
			t.Fatal("失效原文仍可用或错误类型异常", err)
		}
	}
	t.Run("committed rotation and subsequent rotation", func(t *testing.T) {
		original := createFixture(t, 1)
		for iteration := 0; iteration < 2; iteration++ {
			newRaw, newHash := service.GenerateRefreshToken()
			var prepared *domain.VerifiedRefreshSession
			err := store.WithRefreshUserTransaction(ctx, original.verified, func(txContext context.Context, txStore *Store, _ domain.VerifiedRefreshSession) error {
				var err error
				prepared, err = txStore.RotateRefreshToken(txContext, newHash)
				return err
			})
			if err != nil || prepared == nil || prepared.Session != original.verified.Session || prepared.TokenGeneration != original.verified.TokenGeneration+1 || prepared.TokenHash != newHash {
				t.Fatal("真实轮换结果不正确", err)
			}
			assertInvalid(t, original.raw)
			assertCurrent(t, newRaw, *prepared)
			original = fixture{raw: newRaw, verified: *prepared}
		}
	})
	t.Run("callback failure and panic roll back digest and generation", func(t *testing.T) {
		original := createFixture(t, 1)
		failure := errors.New("fixture failure after rotation")
		for _, mode := range []string{"error", "panic", "cancel", "repeat"} {
			t.Run(mode, func(t *testing.T) {
				newRaw, newHash := service.GenerateRefreshToken()
				requestContext, cancelRequest := context.WithCancel(ctx)
				defer cancelRequest()
				action := func(txContext context.Context, txStore *Store, _ domain.VerifiedRefreshSession) error {
					if _, err := txStore.RotateRefreshToken(txContext, newHash); err != nil {
						return fmt.Errorf("夹具首次轮换失败: %w", err)
					}
					switch mode {
					case "panic":
						panic(failure)
					case "cancel":
						cancelRequest()
						return nil
					case "repeat":
						_, nextHash := service.GenerateRefreshToken()
						_, err := txStore.RotateRefreshToken(txContext, nextHash)
						return err
					default:
						return failure
					}
				}
				if mode == "panic" {
					func() {
						defer func() {
							if recover() != failure {
								t.Error("原始 panic 未传播")
							}
						}()
						_ = store.WithRefreshUserTransaction(requestContext, original.verified, action)
					}()
				} else {
					err := store.WithRefreshUserTransaction(requestContext, original.verified, action)
					want := failure
					if mode == "cancel" {
						want = context.Canceled
					}
					if mode == "repeat" {
						want = domain.ErrInvalidRefreshToken
					}
					if !errors.Is(err, want) {
						t.Fatal("轮换后失败的事务未返回预期错误", err)
					}
				}
				assertCurrent(t, original.raw, original.verified)
				assertInvalid(t, newRaw)
			})
		}
	})
	t.Run("digest collision with another user rolls back", func(t *testing.T) {
		original, another := createFixture(t, 1), createFixture(t, 3)
		err := store.WithRefreshUserTransaction(ctx, original.verified, func(txContext context.Context, txStore *Store, _ domain.VerifiedRefreshSession) error {
			result, err := txStore.RotateRefreshToken(txContext, another.verified.TokenHash)
			if result != nil {
				t.Error("摘要冲突返回了部分结果")
			}
			return err
		})
		if !errors.Is(err, domain.ErrRefreshTokenConflict) {
			t.Fatal("真实唯一键冲突未转换", err)
		}
		assertCurrent(t, original.raw, original.verified)
		assertCurrent(t, another.raw, another.verified)
	})
	t.Run("maximum generation is rejected before overflow", func(t *testing.T) {
		original := createFixture(t, 1)
		if _, err := pool.ExecContext(ctx, "UPDATE auth_sessions SET token_generation=? WHERE id=?", ^uint64(0)-1, original.verified.Session.ID); err != nil {
			t.Fatal(err)
		}
		current, err := verifier.Verify(ctx, original.raw)
		if err != nil || current == nil {
			t.Fatal("最大代次夹具读取失败", err)
		}
		newRaw, newHash := service.GenerateRefreshToken()
		var maximum *domain.VerifiedRefreshSession
		err = store.WithRefreshUserTransaction(ctx, *current, func(txContext context.Context, txStore *Store, _ domain.VerifiedRefreshSession) error {
			var err error
			maximum, err = txStore.RotateRefreshToken(txContext, newHash)
			return err
		})
		if err != nil || maximum == nil || maximum.TokenGeneration != ^uint64(0) {
			t.Fatal("完整 uint64 最大代次写入失败", err)
		}
		assertCurrent(t, newRaw, *maximum)
		assertInvalid(t, original.raw)
		_, nextHash := service.GenerateRefreshToken()
		err = store.WithRefreshUserTransaction(ctx, *maximum, func(txContext context.Context, txStore *Store, _ domain.VerifiedRefreshSession) error {
			_, err := txStore.RotateRefreshToken(txContext, nextHash)
			return err
		})
		if !errors.Is(err, domain.ErrInvalidSessionData) {
			t.Fatal("最大代次未被拒绝", err)
		}
		assertCurrent(t, newRaw, *maximum)
	})
	t.Run("two requests with the same old credential have one winner", func(t *testing.T) {
		original := createFixture(t, 1)
		type outcome struct {
			raw      string
			prepared *domain.VerifiedRefreshSession
			err      error
		}
		start := make(chan struct{})
		results := make(chan outcome, 2)
		var callbacks atomic.Int32
		for range 2 {
			newRaw, newHash := service.GenerateRefreshToken()
			go func() {
				<-start
				var prepared *domain.VerifiedRefreshSession
				err := store.WithRefreshUserTransaction(ctx, original.verified, func(txContext context.Context, txStore *Store, _ domain.VerifiedRefreshSession) error {
					callbacks.Add(1)
					var err error
					prepared, err = txStore.RotateRefreshToken(txContext, newHash)
					return err
				})
				results <- outcome{raw: newRaw, prepared: prepared, err: err}
			}()
		}
		close(start)
		wins, rejections := 0, 0
		for range 2 {
			select {
			case result := <-results:
				if result.err == nil {
					wins++
					if result.prepared == nil || result.prepared.TokenGeneration != 2 {
						t.Fatal("并发胜者结果不正确")
					}
					assertCurrent(t, result.raw, *result.prepared)
				} else {
					rejections++
					if !errors.Is(result.err, domain.ErrInvalidRefreshToken) || result.prepared != nil {
						t.Fatal("并发旧凭证失败结果不正确", result.err)
					}
					assertInvalid(t, result.raw)
				}
			case <-time.After(6 * time.Second):
				t.Fatal("并发轮换未结束")
			}
		}
		if wins != 1 || rejections != 1 || callbacks.Load() != 1 {
			t.Fatal("同一旧凭证未限制为单次成功")
		}
		assertInvalid(t, original.raw)
	})
}
