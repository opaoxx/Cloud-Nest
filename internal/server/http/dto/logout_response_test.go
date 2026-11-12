package dto

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/service"
)

func TestNewLogoutDataJSON(t *testing.T) {
	// 1. 转换已提交结果，确认原对象和响应数据相互独立。
	result := &domain.LogoutResult{Revoked: true}
	data, err := NewLogoutData(result)
	if err != nil || data == nil || !data.Revoked || !result.Revoked {
		t.Fatal("退出结果转换失败或原对象被修改", err)
	}
	result.Revoked = false
	if !data.Revoked {
		t.Fatal("修改服务结果影响了已经生成的响应数据")
	}

	// 2. 编码完整响应，核对字段数量、布尔类型和请求标识。
	response := LogoutResponse{Data: *data, RequestID: "28ee758a-0727-42de-ae7d-3bb33155e148"}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal("退出响应 JSON 编码失败", err)
	}
	var body map[string]any
	if err := json.Unmarshal(encoded, &body); err != nil || len(body) != 2 || body["request_id"] != response.RequestID {
		t.Fatal("外层退出响应字段错误", err)
	}
	object, valid := body["data"].(map[string]any)
	if !valid || len(object) != 1 || object["revoked"] != true {
		t.Fatal("撤销状态的字段、数量或布尔类型错误")
	}

	// 3. 多次转换产生独立数据，外层响应保留组装时的值。
	result.Revoked = true
	second, err := NewLogoutData(result)
	if err != nil || second == nil || second == data || !second.Revoked {
		t.Fatal("再次转换未生成独立的正确结果", err)
	}
	data.Revoked = false
	if !result.Revoked || !second.Revoked || !response.Data.Revoked {
		t.Fatal("修改 DTO 影响了服务结果、其他 DTO 或已组装响应")
	}
}

func TestNewLogoutDataFailures(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		result *domain.LogoutResult
	}{
		{name: "nil result"},
		{name: "unconfirmed result", result: &domain.LogoutResult{}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			data, err := NewLogoutData(testCase.result)
			if data != nil || !errors.Is(err, domain.ErrInvalidSessionData) {
				t.Fatal("异常结果未拒绝或错误类型不正确", err)
			}
			if testCase.result != nil && testCase.result.Revoked {
				t.Fatal("校验失败时修改了输入")
			}
		})
	}
}

// logoutDataTransaction 用调用顺序记录模拟事务中的撤销能力。
type logoutDataTransaction struct {
	events *[]string
}

func (transaction logoutDataTransaction) RevokeCurrentSession(context.Context) error {
	*transaction.events = append(*transaction.events, "revoke")
	return nil
}

func TestNewLogoutDataServiceResult(t *testing.T) {
	for _, commitFails := range []bool{false, true} {
		name := "confirmed commit"
		if commitFails {
			name = "commit failure"
		}
		t.Run(name, func(t *testing.T) {
			// 1. 使用真实退出服务和模拟事务，分别观察提交与提交故障。
			var events []string
			commitFailure := errors.New("fixture commit failure")
			logout := service.NewLogoutService(func(ctx context.Context, _ domain.Actor, action func(context.Context, service.LogoutTransaction) error) error {
				events = append(events, "begin")
				if err := action(ctx, logoutDataTransaction{events: &events}); err != nil {
					return err
				}
				events = append(events, "commit")
				if commitFails {
					return commitFailure
				}
				return nil
			})
			result, logoutError := logout.Logout(context.Background(), domain.Actor{UserID: 7, DeviceID: 13, SessionID: 11})
			if !reflect.DeepEqual(events, []string{"begin", "revoke", "commit"}) {
				t.Fatal("退出服务的调用顺序错误")
			}

			// 2. 转换服务的实际返回值，提交故障的空结果仍被 DTO 拒绝。
			data, dataError := NewLogoutData(result)
			if commitFails {
				if !errors.Is(logoutError, commitFailure) || result != nil || data != nil || !errors.Is(dataError, domain.ErrInvalidSessionData) {
					t.Fatal("提交故障被转换为成功数据", logoutError, dataError)
				}
				return
			}
			if logoutError != nil || dataError != nil || data == nil || !data.Revoked {
				t.Fatal("已提交的退出结果转换失败", logoutError, dataError)
			}
		})
	}
}
