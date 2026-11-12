package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func refreshCredentialFixture() (string, domain.RefreshSession, time.Time) {
	raw := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x19}, 32))
	now := time.Date(2026, 11, 12, 8, 0, 0, 123456000, time.UTC)
	snapshot := domain.RefreshSession{
		SessionAuthorization: domain.SessionAuthorization{UserID: 7, SessionID: 11, DeviceID: 13, UserStatus: "ACTIVE", SessionExpiresAt: now.Add(time.Hour).In(time.FixedZone("UTC+8", 8*60*60))},
		TokenGeneration:      1,
	}
	return raw, snapshot, now
}

func TestRefreshCredentialServiceResults(t *testing.T) {
	fault := errors.New("fixture database failure")
	for _, testCase := range []struct {
		name string
		want error
	}{
		{name: "active"},
		{name: "maximum ids and generation"},
		{name: "not found", want: domain.ErrInvalidRefreshToken},
		{name: "wrapped not found", want: domain.ErrInvalidRefreshToken},
		{name: "query fault", want: fault},
		{name: "nil snapshot", want: domain.ErrInvalidSessionData},
		{name: "zero user", want: domain.ErrInvalidSessionData},
		{name: "zero session", want: domain.ErrInvalidSessionData},
		{name: "zero device", want: domain.ErrInvalidSessionData},
		{name: "zero generation", want: domain.ErrInvalidSessionData},
		{name: "zero expiry", want: domain.ErrInvalidSessionData},
		{name: "disabled", want: domain.ErrAccountDisabled},
		{name: "unknown status", want: domain.ErrInvalidAccountData},
		{name: "revoked session", want: domain.ErrSessionRevoked},
		{name: "revoked device", want: domain.ErrDeviceRevoked},
		{name: "expired", want: domain.ErrSessionExpired},
		{name: "exact boundary", want: domain.ErrSessionExpired},
		{name: "expires during query", want: domain.ErrSessionExpired},
		{name: "disabled before revocations", want: domain.ErrAccountDisabled},
		{name: "session revocation before device", want: domain.ErrSessionRevoked},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			raw, snapshot, now := refreshCredentialFixture()
			switch testCase.name {
			case "maximum ids and generation":
				snapshot.UserID, snapshot.SessionID, snapshot.DeviceID, snapshot.TokenGeneration = ^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0)
			case "zero user":
				snapshot.UserID = 0
			case "zero session":
				snapshot.SessionID = 0
			case "zero device":
				snapshot.DeviceID = 0
			case "zero generation":
				snapshot.TokenGeneration = 0
			case "zero expiry":
				snapshot.SessionExpiresAt = time.Time{}
			case "disabled":
				snapshot.UserStatus = "DISABLED"
			case "unknown status":
				snapshot.UserStatus = "UNKNOWN"
			case "revoked session":
				snapshot.SessionRevokedAt = &now
			case "revoked device":
				snapshot.DeviceRevokedAt = &now
			case "expired":
				snapshot.SessionExpiresAt = now.Add(-time.Nanosecond)
			case "exact boundary":
				snapshot.SessionExpiresAt = now
			case "disabled before revocations":
				snapshot.UserStatus = "DISABLED"
				snapshot.SessionRevokedAt = &now
				snapshot.DeviceRevokedAt = &now
			case "session revocation before device":
				snapshot.SessionRevokedAt = &now
				snapshot.DeviceRevokedAt = &now
			}
			before := snapshot
			ctx := context.Background()
			calls := 0
			verifier := NewRefreshCredentialService(func(received context.Context, hash [sha256.Size]byte) (*domain.RefreshSession, error) {
				calls++
				if received != ctx || hash != sha256.Sum256([]byte(raw)) {
					t.Error("查询未收到原上下文或原文文本的摘要")
				}
				switch testCase.name {
				case "not found":
					return nil, domain.ErrSessionNotFound
				case "wrapped not found":
					return nil, fmt.Errorf("fixture wrap: %w", domain.ErrSessionNotFound)
				case "query fault":
					return nil, fault
				case "nil snapshot":
					return nil, nil
				case "expires during query":
					now = snapshot.SessionExpiresAt
				}
				return &snapshot, nil
			})
			verifier.now = func() time.Time { return now }
			result, err := verifier.Verify(ctx, raw)
			if calls != 1 {
				t.Fatal("合法格式应恰好定位一次会话")
			}
			if testCase.want != nil {
				if result != nil || !errors.Is(err, testCase.want) {
					t.Fatal("拒绝结果错误或丢失原因")
				}
			} else {
				if err != nil || result == nil || result.Session.ID != snapshot.SessionID || result.Session.UserID != snapshot.UserID || result.Session.DeviceID != snapshot.DeviceID || result.TokenGeneration != snapshot.TokenGeneration ||
					!result.Session.ExpiresAt.Equal(snapshot.SessionExpiresAt) || result.Session.ExpiresAt.Location() != time.UTC || result.TokenHash != sha256.Sum256([]byte(raw)) {
					t.Fatal("校验结果标识、期限或摘要错误")
				}
				encoded, encodeError := json.Marshal(result)
				if encodeError != nil || strings.Contains(string(encoded), raw) || strings.Contains(string(encoded), "TokenHash") {
					t.Fatal("内部结果普通序列化泄漏了凭证或摘要")
				}
				result.Session.ID = 99
			}
			if !reflect.DeepEqual(snapshot, before) {
				t.Fatal("服务修改了查询快照")
			}
			if err != nil && strings.Contains(err.Error(), raw) {
				t.Fatal("错误回显了刷新原文")
			}
		})
	}
}

func TestRefreshCredentialServiceTokenFormat(t *testing.T) {
	raw, snapshot, now := refreshCredentialFixture()
	for _, testCase := range []struct {
		name, token string
		valid       bool
	}{
		{name: "canonical", token: raw, valid: true},
		{name: "zero random bytes", token: strings.Repeat("A", 43), valid: true},
		{name: "empty"},
		{name: "short", token: raw[:42]},
		{name: "long", token: raw + "A"},
		{name: "padded", token: raw + "="},
		{name: "leading space", token: " " + raw},
		{name: "trailing space", token: raw + " "},
		{name: "embedded space", token: raw[:20] + " " + raw[21:]},
		{name: "newline ignored by base64 decoder", token: base64.RawURLEncoding.EncodeToString(make([]byte, 31)) + "\n"},
		{name: "noncanonical trailing bits", token: strings.Repeat("A", 42) + "B"},
		{name: "standard alphabet", token: base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{255}, 32))},
		{name: "unicode", token: raw[:40] + "中"},
		{name: "bearer prefix", token: "Bearer " + raw},
		{name: "invalid utf8", token: raw[:42] + "\xff"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			calls := 0
			verifier := NewRefreshCredentialService(func(_ context.Context, hash [sha256.Size]byte) (*domain.RefreshSession, error) {
				calls++
				if hash != sha256.Sum256([]byte(testCase.token)) {
					t.Error("摘要针对的不是原文文本")
				}
				return &snapshot, nil
			})
			verifier.now = func() time.Time { return now }
			result, err := verifier.Verify(context.Background(), testCase.token)
			if testCase.valid {
				if err != nil || result == nil || calls != 1 {
					t.Fatal("规范原文被拒绝")
				}
			} else if !errors.Is(err, domain.ErrInvalidRefreshToken) || result != nil || calls != 0 {
				t.Fatal("错误格式仍查询了数据库或返回了结果")
			}
		})
	}
}

func TestRefreshCredentialServiceCancellation(t *testing.T) {
	for _, phase := range []string{"nil context", "before query", "after query", "before return", "deadline"} {
		t.Run(phase, func(t *testing.T) {
			raw, snapshot, now := refreshCredentialFixture()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls, expected := 0, 1
			want := error(context.Canceled)
			switch phase {
			case "nil context":
				ctx = nil
				expected = 0
				want = nil
			case "before query":
				cancel()
				expected = 0
			case "deadline":
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 20*time.Millisecond)
				defer stop()
				want = context.DeadlineExceeded
			}
			verifier := NewRefreshCredentialService(func(ctx context.Context, _ [sha256.Size]byte) (*domain.RefreshSession, error) {
				calls++
				if phase == "deadline" {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				if phase == "after query" {
					cancel()
				}
				return &snapshot, nil
			})
			verifier.now = func() time.Time {
				if phase == "before return" {
					cancel()
				}
				return now
			}
			result, err := verifier.Verify(ctx, raw)
			if err == nil || result != nil || calls != expected || want != nil && !errors.Is(err, want) {
				t.Fatal("上下文错误未在约定阶段结束处理")
			}
		})
	}
}

func TestRefreshCredentialServiceDependency(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("缺少查询方法未触发装配错误")
		}
	}()
	NewRefreshCredentialService(nil)
}

func TestRefreshCredentialServiceGeneratedToken(t *testing.T) {
	raw, hash := GenerateRefreshToken()
	_, snapshot, now := refreshCredentialFixture()
	verifier := NewRefreshCredentialService(func(_ context.Context, received [sha256.Size]byte) (*domain.RefreshSession, error) {
		if received != hash {
			t.Error("实际生成令牌的入库摘要和校验摘要不一致")
		}
		return &snapshot, nil
	})
	verifier.now = func() time.Time { return now }
	result, err := verifier.Verify(context.Background(), raw)
	if err != nil || result == nil || result.TokenHash != hash {
		t.Fatal("实际刷新凭证未通过校验")
	}
}
