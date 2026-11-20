package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestEntryDownloadServiceCreate(t *testing.T) {
	targetUnavailable := domain.ErrDownloadTargetUnavailable
	storageUnavailable := domain.ErrDownloadStorageUnavailable
	staleVersion := &domain.EntryRevisionConflictError{CurrentRevision: 4}
	commitFailure := errors.New("fixture access commit failure")
	for _, scenario := range []struct {
		name, purpose, storedMIME, wantMIME, wantDisposition string
		targetError, objectError, signError, wantError       error
		accessError, commitError                             error
		editTarget                                           func(*domain.EntryDownloadTarget) *domain.EntryDownloadTarget
		editLink                                             func(*domain.SignedDownloadURL) *domain.SignedDownloadURL
		cancelAfter                                          string
		wantCalls                                            []string
	}{
		{name: "download falls back", purpose: "download", wantMIME: "application/octet-stream",
			wantDisposition: `attachment; filename="download.png"; filename*=UTF-8''%E5%9B%BE%E7%89%87.png`,
			wantCalls:       []string{"target", "check", "sign", "access"}},
		{name: "preview", purpose: "preview", storedMIME: "IMAGE/PNG", wantMIME: "image/png",
			wantDisposition: `inline; filename="download.png"; filename*=UTF-8''%E5%9B%BE%E7%89%87.png`,
			wantCalls:       []string{"target", "check", "sign", "access"}},
		{name: "invalid purpose", purpose: "open", wantError: domain.ErrInvalidDownloadPurpose},
		{name: "missing entry", targetError: domain.ErrFileEntryNotFound, wantError: domain.ErrFileEntryNotFound, wantCalls: []string{"target"}},
		{name: "stale revision", targetError: staleVersion, wantError: staleVersion, wantCalls: []string{"target"}},
		{name: "nil target", editTarget: func(*domain.EntryDownloadTarget) *domain.EntryDownloadTarget { return nil },
			wantError: domain.ErrInvalidFileEntryData, wantCalls: []string{"target"}},
		{name: "wrong owner", editTarget: func(target *domain.EntryDownloadTarget) *domain.EntryDownloadTarget { target.UserID++; return target },
			wantError: domain.ErrInvalidFileEntryData, wantCalls: []string{"target"}},
		{name: "wrong revision in result", editTarget: func(target *domain.EntryDownloadTarget) *domain.EntryDownloadTarget { target.Revision++; return target },
			wantError: domain.ErrInvalidFileEntryData, wantCalls: []string{"target"}},
		{name: "unsupported preview", purpose: "preview", storedMIME: "image/svg+xml",
			wantError: domain.ErrUnsupportedPreview, wantCalls: []string{"target"}},
		{name: "invalid saved filename", editTarget: func(target *domain.EntryDownloadTarget) *domain.EntryDownloadTarget {
			target.Name = "path/name"
			return target
		},
			wantError: domain.ErrInvalidEntryName, wantCalls: []string{"target"}},
		{name: "object missing", objectError: targetUnavailable, wantError: targetUnavailable, wantCalls: []string{"target", "check"}},
		{name: "dependency unavailable", objectError: storageUnavailable, wantError: storageUnavailable, wantCalls: []string{"target", "check"}},
		{name: "signing failure", signError: storageUnavailable, wantError: storageUnavailable, wantCalls: []string{"target", "check", "sign"}},
		{name: "missing signature result", editLink: func(*domain.SignedDownloadURL) *domain.SignedDownloadURL { return nil },
			wantError: storageUnavailable, wantCalls: []string{"target", "check", "sign"}},
		{name: "missing expiry", editLink: func(link *domain.SignedDownloadURL) *domain.SignedDownloadURL {
			link.ExpiresAt = time.Time{}
			return link
		},
			wantError: storageUnavailable, wantCalls: []string{"target", "check", "sign"}},
		{name: "invalid address scheme", editLink: func(link *domain.SignedDownloadURL) *domain.SignedDownloadURL {
			link.URL = "file:///object"
			return link
		},
			wantError: storageUnavailable, wantCalls: []string{"target", "check", "sign"}},
		{name: "cancel after reading", cancelAfter: "target", wantError: context.Canceled, wantCalls: []string{"target"}},
		{name: "cancel after HEAD", cancelAfter: "check", objectError: targetUnavailable,
			wantError: context.Canceled, wantCalls: []string{"target", "check"}},
		{name: "cancel after signing", cancelAfter: "sign", wantError: context.Canceled, wantCalls: []string{"target", "check", "sign"}},
		{name: "revoked session before access", accessError: domain.ErrSessionRevoked, wantError: domain.ErrSessionRevoked,
			wantCalls: []string{"target", "check", "sign", "access"}},
		{name: "access commit failed", commitError: commitFailure, wantError: commitFailure,
			wantCalls: []string{"target", "check", "sign", "access"}},
		{name: "cancel after access", cancelAfter: "access commit", wantError: context.Canceled,
			wantCalls: []string{"target", "check", "sign", "access"}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// 1. 准备可控的读取、签名与访问事务，记录调用顺序。
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			actor := domain.Actor{UserID: 7, DeviceID: 19, SessionID: 17}
			const entryID, revision = uint64(13), uint64(3)
			const signedURL = "https://storage.example.invalid/bucket/object?X-Amz-Signature=fixture"
			digest := sha256.Sum256([]byte("fixture"))
			target := &domain.EntryDownloadTarget{EntryID: entryID, UserID: actor.UserID, Revision: revision, ObjectID: 41,
				Name: "图片.png", SizeBytes: 7, VerifiedSHA256: digest}
			if scenario.storedMIME != "" {
				target.MIMEType = new(scenario.storedMIME)
			}
			if scenario.editTarget != nil {
				target = scenario.editTarget(target)
			}
			expiresAt := time.Date(2026, 11, 20, 8, 15, 0, 0, time.FixedZone("UTC+8", 8*60*60))
			link := &domain.SignedDownloadURL{URL: signedURL, ExpiresAt: expiresAt}
			if scenario.editLink != nil {
				link = scenario.editLink(link)
			}
			_, accessStore := recordAccessServiceFixture()
			access := &downloadAccessTransactionStub{
				recordAccessTransactionStub: accessStore,
				content:                     &domain.FileEntryContent{ID: entryID, UserID: actor.UserID, ObjectID: new(uint64(41))},
			}
			var calls []string
			record := func(stage string) {
				calls = append(calls, stage)
				if stage == scenario.cancelAfter {
					cancel()
				}
			}
			service := NewEntryDownloadService(
				func(receivedContext context.Context, identity domain.Actor, targetID, expectedRevision uint64) (*domain.EntryDownloadTarget, error) {
					record("target")
					if receivedContext != ctx || identity != actor || targetID != entryID || expectedRevision != revision {
						t.Error("目标查询未收到认证身份及指定版本")
					}
					return target, scenario.targetError
				},
				func(receivedContext context.Context, checked *domain.EntryDownloadTarget) error {
					record("check")
					if receivedContext != ctx || checked != target {
						t.Error("对象检查未接收同一目标")
					}
					return scenario.objectError
				},
				func(receivedContext context.Context, signed *domain.EntryDownloadTarget, contentType, disposition string) (*domain.SignedDownloadURL, error) {
					record("sign")
					if receivedContext != ctx || signed != target || (scenario.wantMIME != "" &&
						(contentType != scenario.wantMIME || disposition != scenario.wantDisposition)) {
						t.Error("签名目标、媒体类型或文件名不符")
					}
					return link, scenario.signError
				},
				func(receivedContext context.Context, identity domain.Actor, action func(context.Context, DownloadAccessTransaction) error) error {
					record("access")
					if receivedContext != ctx || identity != actor {
						t.Error("访问事务未取得当前请求及认证身份")
					}
					if scenario.accessError != nil {
						return scenario.accessError
					}
					if err := action(ctx, access); err != nil {
						return err
					}
					if access.writeCalls != 1 {
						t.Error("事务未写入最近访问")
					}
					if scenario.cancelAfter == "access commit" {
						cancel()
					}
					return scenario.commitError
				},
			)
			purpose := scenario.purpose
			if purpose == "" {
				purpose = "download"
			}

			// 2. 核对提前停止及独立的公开内容事实。
			result, err := service.Create(ctx, actor, entryID, revision, purpose)
			if !reflect.DeepEqual(calls, scenario.wantCalls) || !errors.Is(err, scenario.wantError) {
				t.Fatal("下载授权调用或错误不符合预期", calls, err)
			}
			if scenario.wantError != nil {
				if result != nil {
					t.Fatal("失败时仍返回了下载授权")
				}
				return
			}
			want := &domain.EntryDownloadAuthorization{
				Link: domain.SignedDownloadURL{URL: signedURL, ExpiresAt: expiresAt.UTC()}, ObjectID: 41, Revision: revision,
				SizeBytes: 7, VerifiedSHA256: digest, MIMEType: scenario.wantMIME, Disposition: scenario.wantDisposition,
			}
			*link = domain.SignedDownloadURL{}
			target.VerifiedSHA256[0] ^= 0xff
			if !reflect.DeepEqual(result, want) {
				t.Fatal("下载授权未独立保存签名与内容事实")
			}
		})
	}
}

func TestEntryDownloadServiceInvalidInput(t *testing.T) {
	actor := domain.Actor{UserID: 7, DeviceID: 19, SessionID: 17}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	service := NewEntryDownloadService(
		func(context.Context, domain.Actor, uint64, uint64) (*domain.EntryDownloadTarget, error) {
			t.Fatal("无效输入触发了目标查询")
			return nil, nil
		}, func(context.Context, *domain.EntryDownloadTarget) error { return nil },
		func(context.Context, *domain.EntryDownloadTarget, string, string) (*domain.SignedDownloadURL, error) {
			return nil, nil
		},
		func(context.Context, domain.Actor, func(context.Context, DownloadAccessTransaction) error) error {
			t.Fatal("无效输入开启了访问事务")
			return nil
		},
	)
	for _, scenario := range []struct {
		ctx               context.Context
		actor             domain.Actor
		entryID, revision uint64
	}{
		{nil, actor, 13, 3},
		{t.Context(), domain.Actor{}, 13, 3},
		{t.Context(), actor, 0, 3},
		{t.Context(), actor, 13, 0},
		{cancelled, actor, 13, 3},
	} {
		if result, err := service.Create(scenario.ctx, scenario.actor, scenario.entryID, scenario.revision, "download"); result != nil || err == nil ||
			(scenario.ctx == cancelled && !errors.Is(err, context.Canceled)) {
			t.Fatal("无效输入或取消请求未返回预期错误", err)
		}
	}
}
