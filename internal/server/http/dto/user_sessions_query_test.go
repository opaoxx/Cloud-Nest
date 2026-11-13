package dto

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestParseUserSessionListQuery(t *testing.T) {
	for _, testCase := range []struct {
		name, raw string
		beforeID  uint64
		pageSize  int
	}{
		{name: "defaults", pageSize: 50},
		{name: "explicit first page", raw: "before_id=0", pageSize: 50},
		{name: "smallest boundary", raw: "before_id=1", beforeID: 1, pageSize: 50},
		{name: "page size one", raw: "limit=1", pageSize: 1},
		{name: "page size one hundred", raw: "limit=100", pageSize: 100},
		{name: "both parameters", raw: "before_id=13&limit=2", beforeID: 13, pageSize: 2},
		{name: "full unsigned boundary", raw: "before_id=18446744073709551615&limit=100", beforeID: ^uint64(0), pageSize: 100},
		{name: "url encoded key and value", raw: "before%5Fid=%31%33&limit=%35%30", beforeID: 13, pageSize: 50},
		{name: "extra query fields", raw: "user_id=99&session_id=private-session&note=private-note&limit=2", pageSize: 2},
		{name: "extra repeated fields", raw: "note=a&note=b", pageSize: 50},
		{name: "case sensitive names", raw: "LIMIT=1&BEFORE_ID=13", pageSize: 50},
		{name: "empty separators", raw: "&&", pageSize: 50},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actual, fieldError := ParseUserSessionListQuery(testCase.raw)
			if fieldError != nil || actual == nil || actual.BeforeSessionID != testCase.beforeID || actual.PageSize != testCase.pageSize {
				t.Fatal("查询参数或默认值转换不正确", fieldError)
			}
		})
	}
	first, _ := ParseUserSessionListQuery("")
	second, _ := ParseUserSessionListQuery("")
	first.BeforeSessionID, first.PageSize = 13, 1
	if first == second || !reflect.DeepEqual(second, &UserSessionListQuery{PageSize: 50}) {
		t.Fatal("两次解析共享了可修改的结果")
	}
}

func TestParseUserSessionListQueryFailures(t *testing.T) {
	for _, testCase := range []struct {
		name, raw, field string
	}{
		{name: "empty boundary", raw: "before_id=", field: "before_id"},
		{name: "boundary without equals", raw: "before_id", field: "before_id"},
		{name: "boundary leading zero", raw: "before_id=013", field: "before_id"},
		{name: "boundary padded zero", raw: "before_id=00", field: "before_id"},
		{name: "boundary plus sign", raw: "before_id=%2B13", field: "before_id"},
		{name: "boundary negative", raw: "before_id=-1", field: "before_id"},
		{name: "boundary whitespace", raw: "before_id=%2013", field: "before_id"},
		{name: "boundary decimal", raw: "before_id=1.5", field: "before_id"},
		{name: "boundary exponent", raw: "before_id=1e2", field: "before_id"},
		{name: "boundary hex", raw: "before_id=0x10", field: "before_id"},
		{name: "boundary full width digits", raw: "before_id=１３", field: "before_id"},
		{name: "boundary overflow", raw: "before_id=18446744073709551616", field: "before_id"},
		{name: "boundary duplicate", raw: "before_id=13&before_id=13", field: "before_id"},
		{name: "encoded boundary duplicate", raw: "before_id=13&before%5Fid=11", field: "before_id"},
		{name: "empty limit", raw: "limit=", field: "limit"},
		{name: "limit without equals", raw: "limit", field: "limit"},
		{name: "zero limit", raw: "limit=0", field: "limit"},
		{name: "negative limit", raw: "limit=-1", field: "limit"},
		{name: "excessive limit", raw: "limit=101", field: "limit"},
		{name: "limit leading zero", raw: "limit=050", field: "limit"},
		{name: "limit plus sign", raw: "limit=%2B50", field: "limit"},
		{name: "limit whitespace", raw: "limit=50%20", field: "limit"},
		{name: "limit plus decoded as space", raw: "limit=+50", field: "limit"},
		{name: "limit decimal", raw: "limit=50.0", field: "limit"},
		{name: "limit overflow", raw: "limit=18446744073709551616", field: "limit"},
		{name: "limit duplicate", raw: "limit=50&limit=50", field: "limit"},
		{name: "boundary validation first", raw: "before_id=private-boundary&limit=private-limit", field: "before_id"},
		{name: "invalid escape", raw: "limit=%ZZ", field: "query"},
		{name: "malformed extra field", raw: "limit=1&note=private-%ZZ", field: "query"},
		{name: "semicolon separator", raw: "before_id=13;limit=1", field: "query"},
		{name: "parse error precedes field error", raw: "before_id=private-value&limit=%ZZ", field: "query"},
		{name: "large numeric input", raw: "limit=" + strings.Repeat("9", 4096), field: "limit"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actual, fieldError := ParseUserSessionListQuery(testCase.raw)
			if actual != nil || fieldError == nil || fieldError.Field != testCase.field || fieldError.Message == "" || strings.Contains(fieldError.Message, "private-") {
				t.Fatal("错误参数未拒绝、返回了部分结果或回显输入")
			}
		})
	}
}

func TestParseUserSessionListQueryServiceParameters(t *testing.T) {
	input, fieldError := ParseUserSessionListQuery("before_id=18446744073709551615&limit=1&user_id=99")
	if fieldError != nil {
		t.Fatal("合法参数解析失败", fieldError)
	}
	fixture := userSessionListDataFixture()
	ctx := context.Background()
	sessions := service.NewUserSessionService(func(received context.Context, userID, beforeID uint64, pageSize int) (*domain.UserSessionPage, error) {
		if received != ctx || userID != 7 || beforeID != ^uint64(0) || pageSize != 1 {
			t.Error("解析结果未准确交给服务，或身份被查询参数影响")
		}
		return &domain.UserSessionPage{Sessions: []domain.UserSession{fixture.Sessions[1].UserSession}}, nil
	})
	result, err := sessions.List(ctx, domain.Actor{UserID: 7, SessionID: 11, DeviceID: 13}, input.BeforeSessionID, input.PageSize)
	if err != nil {
		t.Fatal("类型化参数调用服务失败", err)
	}
	data, err := NewUserSessionListData(result)
	if err != nil || data == nil || len(data.Items) != 1 || data.Items[0].ID != "11" || !data.Items[0].Current || data.HasMore {
		t.Fatal("查询参数、服务及响应 DTO 衔接失败", err)
	}
}
