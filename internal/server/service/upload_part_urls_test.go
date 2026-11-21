package service

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestUploadPartURLServiceCreate(t *testing.T) {
	epoch := time.Now().UTC().Truncate(time.Second)
	deadline, linkExpiry := epoch.Add(time.Hour), epoch.Add(time.Minute)
	actor := domain.Actor{UserID: 7, DeviceID: 31, SessionID: 41}
	const key, uploadID = "users/7/objects/fixture", "opaque+A/="
	parts := []UploadPartDeclaration{{PartNumber: 2, SHA256: signingFixtureHex}, {PartNumber: 1, SHA256: signingFixtureHex}}
	signFailure := errors.New("fixture signer error")
	for _, test := range []struct {
		name      string
		change    func(*UploadPartSigningTarget)
		checkErr  error
		signError error
		linkURL   string
		linkEnd   time.Time
		cancelAt  string
		invalid   bool
		want      error
		wantCalls []string
	}{
		{name: "success in request order", wantCalls: []string{"target", "2", "1", "target"}},
		{name: "invalid declaration", invalid: true, wantCalls: []string{"target"}},
		{name: "second part fails", signError: signFailure, want: signFailure, wantCalls: []string{"target", "2", "1"}},
		{name: "signer returned invalid URL", linkURL: "https://name:secret@objects.example.test/fixture", want: domain.ErrUploadStorageUnavailable, wantCalls: []string{"target", "2"}},
		{name: "signer exceeded task deadline", linkEnd: deadline.Add(time.Second), want: domain.ErrUploadStorageUnavailable, wantCalls: []string{"target", "2"}},
		{name: "link expired by delivery", linkEnd: epoch, want: domain.ErrUploadTaskNotReady, wantCalls: []string{"target", "2", "1", "target"}},
		{name: "session changed", change: func(target *UploadPartSigningTarget) { target.UploadID = "new-session" }, want: domain.ErrUploadTaskNotReady, wantCalls: []string{"target", "2", "1", "target"}},
		{name: "object changed", change: func(target *UploadPartSigningTarget) { target.StagedObjectID++ }, want: domain.ErrUploadTaskNotReady, wantCalls: []string{"target", "2", "1", "target"}},
		{name: "key changed", change: func(target *UploadPartSigningTarget) { target.ObjectKey += "-new" }, want: domain.ErrUploadTaskNotReady, wantCalls: []string{"target", "2", "1", "target"}},
		{name: "lease version changed", change: func(target *UploadPartSigningTarget) { target.LeaseVersion++ }, want: domain.ErrUploadTaskNotReady, wantCalls: []string{"target", "2", "1", "target"}},
		{name: "task deadline changed", change: func(target *UploadPartSigningTarget) { target.ExpiresAt = deadline.Add(time.Hour) }, want: domain.ErrUploadTaskNotReady, wantCalls: []string{"target", "2", "1", "target"}},
		{name: "plan changed", change: func(target *UploadPartSigningTarget) { target.FileSizeBytes++ }, want: domain.ErrUploadTaskNotReady, wantCalls: []string{"target", "2", "1", "target"}},
		{name: "active lease at recheck", checkErr: domain.ErrUploadTaskBusy, want: domain.ErrUploadTaskBusy, wantCalls: []string{"target", "2", "1", "target"}},
		{name: "cancelled before reading", cancelAt: "before", want: context.Canceled},
		{name: "cancelled during signing", cancelAt: "sign", want: context.Canceled, wantCalls: []string{"target", "2"}},
		{name: "cancelled during final read", cancelAt: "recheck", want: context.Canceled, wantCalls: []string{"target", "2", "1", "target"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.cancelAt == "before" {
				cancel()
			}
			target := &UploadPartSigningTarget{TaskID: 19, UserID: 7, StagedObjectID: 23, LeaseVersion: 3, ObjectKey: key,
				UploadID: uploadID, FileSizeBytes: signingFixturePartSize + 1, PartSizeBytes: signingFixturePartSize, PartCount: 2, ExpiresAt: deadline}
			var calls []string
			urls := NewUploadPartURLService(
				func(callContext context.Context, current domain.Actor, taskID uint64) (*UploadPartSigningTarget, error) {
					calls = append(calls, "target")
					if callContext != ctx || current != actor || taskID != 19 {
						t.Fatal("签名目标未使用相同认证身份和任务")
					}
					if len(calls) > 1 {
						if test.cancelAt == "recheck" {
							cancel()
						}
						if test.change != nil {
							test.change(target)
						}
						return target, test.checkErr
					}
					return target, nil
				},
				func(callContext context.Context, objectKey, session string, number uint32, size uint64, checksum string, expiresAt time.Time) (*domain.SignedUploadPartURL, error) {
					numberText := strconv.FormatUint(uint64(number), 10)
					calls = append(calls, numberText)
					expectedSize := uint64(signingFixturePartSize)
					if number == 2 {
						expectedSize = 1
					}
					if callContext != ctx || objectKey != key || session != uploadID || size != expectedSize ||
						checksum != signingFixtureBase64 || !expiresAt.Equal(deadline) {
						t.Fatal("签名单片参数与当前目标不一致")
					}
					if test.cancelAt == "sign" {
						cancel()
					}
					if number == 1 && test.signError != nil {
						return nil, test.signError
					}
					link := domain.SignedUploadPartURL{URL: "https://objects.example.test/fixture?partNumber=" + numberText, ExpiresAt: linkExpiry}
					if test.linkURL != "" {
						link.URL = test.linkURL
					}
					if !test.linkEnd.IsZero() {
						link.ExpiresAt = test.linkEnd
					}
					return &link, nil
				},
				func(context.Context, domain.Actor, uint64, time.Time,
					func(*domain.UploadTask, *domain.UserProfile, time.Time, time.Time) (time.Time, error),
				) (*domain.UploadTask, error) {
					t.Fatal("普通签发调用了续期事务")
					return nil, nil
				},
			)
			declarations := parts
			if test.invalid {
				declarations = []UploadPartDeclaration{parts[0], {PartNumber: 1, SHA256: "bad-digest"}}
			}
			result, err := urls.Create(ctx, actor, 19, declarations, nil)
			if !reflect.DeepEqual(calls, test.wantCalls) {
				t.Fatal("调用顺序不符", calls, test.wantCalls)
			}
			if test.invalid {
				var inputError *UploadPartDeclarationError
				if result != nil || !errors.As(err, &inputError) || inputError.Field != "parts.1.sha256" {
					t.Fatal("异常声明未按字段拒绝", err)
				}
				return
			}
			if !errors.Is(err, test.want) {
				t.Fatal("签发结果不符", err, test.want)
			}
			if test.want != nil {
				if result != nil {
					t.Fatal("失败时交付了部分签名")
				}
				return
			}
			want := &UploadPartURLBatch{ExpiresAt: deadline, Parts: []UploadPartURL{
				{PartNumber: 2, SizeBytes: 1, ChecksumSHA256: signingFixtureBase64, URL: "https://objects.example.test/fixture?partNumber=2", URLExpiresAt: linkExpiry},
				{PartNumber: 1, SizeBytes: signingFixturePartSize, ChecksumSHA256: signingFixtureBase64, URL: "https://objects.example.test/fixture?partNumber=1", URLExpiresAt: linkExpiry},
			}}
			if !reflect.DeepEqual(result, want) {
				t.Fatal("签发顺序、片长、摘要或截止时间不符")
			}
		})
	}
}

func TestUploadPartURLServiceCreateWithRenewal(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	actor := domain.Actor{UserID: 7, DeviceID: 31, SessionID: 41}
	const objectKey = "users/7/objects/renewal-fixture"
	for _, test := range []struct {
		name           string
		remaining      time.Duration
		invalidPart    bool
		staleExpiry    bool
		changeLease    bool
		cancelOnCommit bool
		failSigning    bool
		want           error
		wantCalls      []string
	}{
		{name: "renew before signing", remaining: time.Hour, wantCalls: []string{"target", "renew", "2", "1", "target"}},
		{name: "keep later deadline", remaining: 25 * time.Hour, wantCalls: []string{"target", "renew", "2", "1", "target"}},
		{name: "reject bad second declaration", remaining: time.Hour, invalidPart: true, wantCalls: []string{"target"}},
		{name: "reject stale expiry before write", remaining: time.Hour, staleExpiry: true, want: domain.ErrUploadTaskNotReady, wantCalls: []string{"target"}},
		{name: "reject changed binding under lock", remaining: time.Hour, changeLease: true, want: domain.ErrUploadTaskNotReady, wantCalls: []string{"target", "renew"}},
		{name: "cancelled after commit", remaining: time.Hour, cancelOnCommit: true, want: context.Canceled, wantCalls: []string{"target", "renew"}},
		{name: "signing fails after commit", remaining: time.Hour, failSigning: true, want: domain.ErrUploadStorageUnavailable, wantCalls: []string{"target", "renew", "2", "1"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			oldExpiry := now.Add(test.remaining)
			current := uploadPartSigningTaskFixture(oldExpiry)
			account := domain.UserProfile{ID: current.UserID, Status: "ACTIVE", QuotaBytes: current.FileSize, ReservedBytes: current.FileSize}
			var calls []string
			urls := NewUploadPartURLService(
				func(context.Context, domain.Actor, uint64) (*UploadPartSigningTarget, error) {
					calls = append(calls, "target")
					return &UploadPartSigningTarget{TaskID: current.ID, UserID: current.UserID, StagedObjectID: current.StagedObjectID,
						LeaseVersion: current.LeaseVersion, ObjectKey: objectKey, UploadID: *current.StorageUploadID,
						FileSizeBytes: current.FileSize, PartSizeBytes: *current.PartSize, PartCount: *current.PartCount, ExpiresAt: current.ExpiresAt}, nil
				},
				func(_ context.Context, key, session string, number uint32, _ uint64, _ string, expiresAt time.Time) (*domain.SignedUploadPartURL, error) {
					calls = append(calls, strconv.FormatUint(uint64(number), 10))
					if key != objectKey || session != *current.StorageUploadID || !expiresAt.Equal(current.ExpiresAt) {
						t.Fatal("签名没有使用已提交的绑定和期限")
					}
					if test.failSigning && number == 1 {
						return nil, domain.ErrUploadStorageUnavailable
					}
					return &domain.SignedUploadPartURL{URL: "https://objects.example.test/fixture", ExpiresAt: now.Add(time.Minute)}, nil
				},
				func(callContext context.Context, currentActor domain.Actor, taskID uint64, expected time.Time,
					resolve func(*domain.UploadTask, *domain.UserProfile, time.Time, time.Time) (time.Time, error),
				) (*domain.UploadTask, error) {
					calls = append(calls, "renew")
					if callContext != ctx || currentActor != actor || taskID != current.ID || !expected.Equal(oldExpiry) {
						t.Fatal("续期事务未收到原身份、任务和旧期限")
					}
					locked := *current
					if test.changeLease {
						locked.LeaseVersion++
					}
					expiry, err := resolve(&locked, &account, expected, now)
					if err != nil {
						return nil, err
					}
					current.ExpiresAt = expiry
					if test.cancelOnCommit {
						cancel()
					}
					return current, nil
				},
			)
			parts := []UploadPartDeclaration{{PartNumber: 2, SHA256: signingFixtureHex}, {PartNumber: 1, SHA256: signingFixtureHex}}
			if test.invalidPart {
				parts[1].SHA256 = "bad-digest"
			}
			expected := oldExpiry
			if test.staleExpiry {
				expected = expected.Add(-time.Microsecond)
			}
			result, err := urls.Create(ctx, actor, current.ID, parts, &expected)
			if !reflect.DeepEqual(calls, test.wantCalls) {
				t.Fatal("调用顺序不符", calls, test.wantCalls)
			}
			if test.invalidPart {
				var fieldError *UploadPartDeclarationError
				if result != nil || !errors.As(err, &fieldError) || fieldError.Field != "parts.1.sha256" || !current.ExpiresAt.Equal(oldExpiry) {
					t.Fatal("无效分片声明影响了任务期限", err)
				}
				return
			}
			wantExpiry := oldExpiry
			if !test.staleExpiry && !test.changeLease && wantExpiry.Before(now.Add(24*time.Hour)) {
				wantExpiry = now.Add(24 * time.Hour)
			}
			if !errors.Is(err, test.want) || !current.ExpiresAt.Equal(wantExpiry) {
				t.Fatal("签发结果或任务期限不符", err, current.ExpiresAt, wantExpiry)
			}
			if test.want != nil {
				if result != nil {
					t.Fatal("失败时仍交付了链接")
				}
				return
			}
			if result == nil || !result.ExpiresAt.Equal(wantExpiry) || len(result.Parts) != 2 {
				t.Fatal("成功结果缺少提交后的期限或分片")
			}
		})
	}
}
