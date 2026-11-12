package dto

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestRefreshRequestValidate(t *testing.T) {
	raw, _ := service.GenerateRefreshToken()
	for _, testCase := range []struct {
		name      string
		request   RefreshRequest
		wantField string
	}{
		{name: "web", request: RefreshRequest{ClientType: "web"}},
		{name: "native generated token", request: RefreshRequest{ClientType: "native", RefreshToken: raw}},
		{name: "native opaque input preserved", request: RefreshRequest{ClientType: "native", RefreshToken: " private-refresh "}},
		{name: "native whitespace goes to credential verification", request: RefreshRequest{ClientType: "native", RefreshToken: " \t\n"}},
		{name: "missing client", wantField: "client_type"},
		{name: "unknown client", request: RefreshRequest{ClientType: "other", RefreshToken: "private-refresh"}, wantField: "client_type"},
		{name: "uppercase client", request: RefreshRequest{ClientType: "WEB"}, wantField: "client_type"},
		{name: "padded client", request: RefreshRequest{ClientType: " native ", RefreshToken: raw}, wantField: "client_type"},
		{name: "web nonempty body token", request: RefreshRequest{ClientType: "web", RefreshToken: raw}, wantField: "refresh_token"},
		{name: "web whitespace body token", request: RefreshRequest{ClientType: "web", RefreshToken: " "}, wantField: "refresh_token"},
		{name: "native missing token", request: RefreshRequest{ClientType: "native"}, wantField: "refresh_token"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			before := testCase.request
			validationError := testCase.request.Validate()
			if testCase.request != before {
				t.Fatal("校验改变了原始字段")
			}
			if testCase.wantField == "" {
				if validationError != nil {
					t.Fatal("合法字段组合被拒绝", validationError)
				}
				return
			}
			if validationError == nil || validationError.Field != testCase.wantField || validationError.Message == "" {
				t.Fatal("字段错误或提示不正确")
			}
			if strings.Contains(validationError.Message, raw) || strings.Contains(validationError.Message, "private-refresh") {
				t.Fatal("字段提示包含刷新原文")
			}
		})
	}
}

func TestRefreshRequestCredentialFormatBoundary(t *testing.T) {
	queries := 0
	credentials := service.NewRefreshCredentialService(func(context.Context, [sha256.Size]byte) (*domain.RefreshSession, error) {
		queries++
		return nil, domain.ErrSessionNotFound
	})
	for _, raw := range []string{" ", " private-refresh ", strings.Repeat("A", 42), strings.Repeat("A", 43) + "=", "private-token-with-invalid-format"} {
		request := RefreshRequest{ClientType: "native", RefreshToken: raw}
		if request.Validate() != nil {
			t.Fatal("非空原文未交给凭证服务检查")
		}
		result, err := credentials.Verify(context.Background(), request.RefreshToken)
		if result != nil || !errors.Is(err, domain.ErrInvalidRefreshToken) {
			t.Fatal("非法凭证格式未被服务拒绝", err)
		}
	}
	if queries != 0 {
		t.Fatal("非法凭证格式进入了数据库查询")
	}
}
