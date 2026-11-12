package security

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/http/dto"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestRefreshTokenExtraction(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		input   dto.RefreshRequest
		cookies []string
		want    string
	}{
		{name: "web", input: dto.RefreshRequest{ClientType: "web"}, cookies: []string{RefreshCookieName + "=private-cookie"}, want: "private-cookie"},
		{name: "web with other cookies", input: dto.RefreshRequest{ClientType: "web"}, cookies: []string{"theme=dark; " + RefreshCookieName + "=private-cookie; other=value"}, want: "private-cookie"},
		{name: "web across headers", input: dto.RefreshRequest{ClientType: "web"}, cookies: []string{"theme=dark", RefreshCookieName + "=private-cookie"}, want: "private-cookie"},
		{name: "web quoted cookie", input: dto.RefreshRequest{ClientType: "web"}, cookies: []string{RefreshCookieName + `="private-cookie"`}, want: "private-cookie"},
		{name: "web quoted value preserves spaces", input: dto.RefreshRequest{ClientType: "web"}, cookies: []string{RefreshCookieName + `=" private-cookie "`}, want: " private-cookie "},
		{name: "web preserves percent text", input: dto.RefreshRequest{ClientType: "web"}, cookies: []string{RefreshCookieName + "=private%2Dcookie"}, want: "private%2Dcookie"},
		{name: "web framing whitespace", input: dto.RefreshRequest{ClientType: "web"}, cookies: []string{" \t" + RefreshCookieName + " =private-cookie\t "}, want: "private-cookie"},
		{name: "unrelated malformed cookie", input: dto.RefreshRequest{ClientType: "web"}, cookies: []string{"other=invalid\\value; " + RefreshCookieName + "=private-cookie"}, want: "private-cookie"},
		{name: "native", input: dto.RefreshRequest{ClientType: "native", RefreshToken: "private-body"}, want: "private-body"},
		{name: "native body chosen with cookie", input: dto.RefreshRequest{ClientType: "native", RefreshToken: "private-body"}, cookies: []string{RefreshCookieName + "=private-cookie"}, want: "private-body"},
		{name: "native original whitespace", input: dto.RefreshRequest{ClientType: "native", RefreshToken: " private-body "}, want: " private-body "},
		{name: "native with malformed duplicate cookies", input: dto.RefreshRequest{ClientType: "native", RefreshToken: "private-body"}, cookies: []string{RefreshCookieName + "=bad\\value; " + RefreshCookieName + "=other"}, want: "private-body"},
		{name: "missing cookie", input: dto.RefreshRequest{ClientType: "web"}},
		{name: "unrelated cookies only", input: dto.RefreshRequest{ClientType: "web"}, cookies: []string{"theme=dark"}},
		{name: "empty value", input: dto.RefreshRequest{ClientType: "web"}, cookies: []string{RefreshCookieName + "="}},
		{name: "quoted empty value", input: dto.RefreshRequest{ClientType: "web"}, cookies: []string{RefreshCookieName + `=""`}},
		{name: "missing equals", input: dto.RefreshRequest{ClientType: "web"}, cookies: []string{RefreshCookieName}},
		{name: "invalid value", input: dto.RefreshRequest{ClientType: "web"}, cookies: []string{RefreshCookieName + "=bad\\value"}},
		{name: "unclosed quote", input: dto.RefreshRequest{ClientType: "web"}, cookies: []string{RefreshCookieName + `="private-cookie`}},
		{name: "uppercase cookie name", input: dto.RefreshRequest{ClientType: "web"}, cookies: []string{strings.ToUpper(RefreshCookieName) + "=private-cookie"}},
		{name: "name suffix", input: dto.RefreshRequest{ClientType: "web"}, cookies: []string{RefreshCookieName + "X=private-cookie"}},
		{name: "duplicate identical", input: dto.RefreshRequest{ClientType: "web"}, cookies: []string{RefreshCookieName + "=private-cookie; " + RefreshCookieName + "=private-cookie"}},
		{name: "duplicate different", input: dto.RefreshRequest{ClientType: "web"}, cookies: []string{RefreshCookieName + "=private-cookie; " + RefreshCookieName + "=second"}},
		{name: "duplicate headers", input: dto.RefreshRequest{ClientType: "web"}, cookies: []string{RefreshCookieName + "=private-cookie", RefreshCookieName + "=second"}},
		{name: "valid and malformed duplicate", input: dto.RefreshRequest{ClientType: "web"}, cookies: []string{RefreshCookieName + "=private-cookie; " + RefreshCookieName + "=bad\\value"}},
		{name: "malformed and valid duplicate", input: dto.RefreshRequest{ClientType: "web"}, cookies: []string{RefreshCookieName + "=bad\\value", RefreshCookieName + "=private-cookie"}},
		{name: "empty and valid duplicate", input: dto.RefreshRequest{ClientType: "web"}, cookies: []string{RefreshCookieName + "=; " + RefreshCookieName + "=private-cookie"}},
		{name: "web nonempty body", input: dto.RefreshRequest{ClientType: "web", RefreshToken: "private-body"}, cookies: []string{RefreshCookieName + "=private-cookie"}},
		{name: "native missing body", input: dto.RefreshRequest{ClientType: "native"}, cookies: []string{RefreshCookieName + "=private-cookie"}},
		{name: "unknown mode", input: dto.RefreshRequest{ClientType: "other", RefreshToken: "private-body"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/?refresh_token=private-query", nil)
			request.Header.Set("Authorization", "Bearer private-header")
			for _, value := range testCase.cookies {
				request.Header.Add("Cookie", value)
			}
			beforeHeader, beforeInput := request.Header.Clone(), testCase.input
			raw, err := RefreshToken(request, testCase.input)
			if testCase.want == "" {
				if raw != "" || !errors.Is(err, domain.ErrInvalidRefreshToken) {
					t.Fatal("无效提取未返回固定错误", err)
				}
				if strings.Contains(err.Error(), "private-") {
					t.Fatal("提取错误包含凭证")
				}
			} else if err != nil || raw != testCase.want {
				t.Fatal("提取原文不正确", err)
			}
			if !reflect.DeepEqual(request.Header, beforeHeader) || testCase.input != beforeInput {
				t.Fatal("提取改变了请求头或 DTO")
			}
		})
	}
	if raw, err := RefreshToken(nil, dto.RefreshRequest{ClientType: "web"}); raw != "" || !errors.Is(err, domain.ErrInvalidRefreshToken) {
		t.Fatal("nil 请求未拒绝")
	}
}

func TestRefreshTokenCredentialService(t *testing.T) {
	raw, hash := service.GenerateRefreshToken()
	for _, clientType := range []string{"web", "native"} {
		t.Run(clientType, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/", nil)
			input := dto.RefreshRequest{ClientType: clientType}
			if clientType == "web" {
				request.Header.Set("Cookie", RefreshCookieName+"="+raw)
			} else {
				input.RefreshToken = raw
			}
			queries := 0
			credentials := service.NewRefreshCredentialService(func(ctx context.Context, received [sha256.Size]byte) (*domain.RefreshSession, error) {
				queries++
				if received != hash {
					t.Error("提取原文的摘要与生成器不一致")
				}
				return &domain.RefreshSession{SessionAuthorization: domain.SessionAuthorization{UserID: 7, SessionID: 11, DeviceID: 13, UserStatus: "ACTIVE", SessionExpiresAt: time.Now().UTC().Add(time.Hour)}, TokenGeneration: 1}, nil
			})
			extracted, err := RefreshToken(request, input)
			if err != nil {
				t.Fatal(err)
			}
			verified, err := credentials.Verify(request.Context(), extracted)
			if err != nil || verified == nil || verified.TokenHash != hash || verified.Session.UserID != 7 || queries != 1 {
				t.Fatal("提取与真实凭证服务组合失败", err)
			}
		})
	}
}
