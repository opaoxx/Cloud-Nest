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

func TestNewRevokeSessionDataJSON(t *testing.T) {
	// 1. 转换服务结果，确认转换没有修改输入且持有值副本。
	result := &domain.RevokeSessionResult{Revoked: true}
	data, err := NewRevokeSessionData(result)
	if err != nil || data == nil || !data.Revoked || !result.Revoked {
		t.Fatal("撤销结果转换失败或输入被修改", err)
	}
	result.Revoked = false
	if !data.Revoked {
		t.Fatal("修改服务结果影响了已生成的响应数据")
	}

	// 2. 编码完整响应，核对 JSON 字段、布尔类型和请求 ID。
	response := RevokeSessionResponse{Data: *data, RequestID: "28ee758a-0727-42de-ae7d-3bb33155e148"}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal("指定会话撤销响应编码失败", err)
	}
	var body map[string]any
	if err := json.Unmarshal(encoded, &body); err != nil || len(body) != 2 || body["request_id"] != response.RequestID {
		t.Fatal("外层 JSON 字段或请求 ID 错误", err)
	}
	object, valid := body["data"].(map[string]any)
	if !valid || len(object) != 1 || object["revoked"] != true {
		t.Fatal("撤销字段、数量或布尔类型错误")
	}

	// 3. 多次转换和外层组装均保留独立数据。
	result.Revoked = true
	second, err := NewRevokeSessionData(result)
	if err != nil || second == nil || second == data || !second.Revoked {
		t.Fatal("多次转换共享了结果对象", err)
	}
	data.Revoked = false
	if !result.Revoked || !second.Revoked || !response.Data.Revoked {
		t.Fatal("修改 DTO 影响了服务结果、其他 DTO 或已组装响应")
	}
}

func TestNewRevokeSessionDataFailures(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		result *domain.RevokeSessionResult
	}{
		{name: "nil result"},
		{name: "unconfirmed result", result: &domain.RevokeSessionResult{}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			data, err := NewRevokeSessionData(testCase.result)
			if data != nil || !errors.Is(err, domain.ErrInvalidSessionData) {
				t.Fatal("异常结果没有返回空数据及既有数据错误", err)
			}
			if testCase.result != nil && testCase.result.Revoked {
				t.Fatal("校验失败时修改了服务结果")
			}
		})
	}
}

type revokeSessionDataTransaction struct {
	revoke func(context.Context, uint64) error
}

func (transaction revokeSessionDataTransaction) RevokeUserSession(ctx context.Context, targetID uint64) error {
	return transaction.revoke(ctx, targetID)
}

func TestNewRevokeSessionDataServiceResult(t *testing.T) {
	failure := errors.New("fixture transaction fault")
	for _, testCase := range []struct {
		name                                            string
		entryError, revokeError, commitError, wantError error
		skipCallback                                    bool
		events                                          []string
	}{
		{name: "confirmed commit", events: []string{"begin", "revoke", "commit"}},
		{name: "caller session missing", entryError: domain.ErrSessionNotFound, wantError: domain.ErrSessionNotFound, events: []string{"begin"}},
		{name: "target session missing", revokeError: domain.ErrSessionNotFound, wantError: domain.ErrTargetSessionNotFound, events: []string{"begin", "revoke", "rollback"}},
		{name: "write failure", revokeError: failure, wantError: failure, events: []string{"begin", "revoke", "rollback"}},
		{name: "commit failure", commitError: failure, wantError: failure, events: []string{"begin", "revoke", "commit"}},
		{name: "callback omitted", skipCallback: true, wantError: domain.ErrInvalidSessionData, events: []string{"begin"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 使用真实撤销服务与内存事务适配器，记录成功和失败的返回。
			ctx := context.Background()
			type transactionKey struct{}
			txContext := context.WithValue(ctx, transactionKey{}, true)
			actor := domain.Actor{UserID: 7, DeviceID: 13, SessionID: 11}
			targetID := ^uint64(0)
			var events []string
			revoke := service.NewRevokeSessionService(func(received context.Context, receivedActor domain.Actor, action func(context.Context, service.RevokeSessionTransaction) error) error {
				events = append(events, "begin")
				if received != ctx || receivedActor != actor {
					t.Error("事务未收到原请求上下文或 Actor")
				}
				if testCase.entryError != nil || testCase.skipCallback {
					return testCase.entryError
				}
				transaction := revokeSessionDataTransaction{revoke: func(received context.Context, receivedTargetID uint64) error {
					events = append(events, "revoke")
					if received != txContext || receivedTargetID != targetID {
						t.Error("撤销未收到事务上下文或完整目标 ID")
					}
					return testCase.revokeError
				}}
				if err := action(txContext, transaction); err != nil {
					events = append(events, "rollback")
					return err
				}
				events = append(events, "commit")
				return testCase.commitError
			})
			result, serviceError := revoke.Revoke(ctx, actor, targetID)
			if !reflect.DeepEqual(events, testCase.events) {
				t.Fatal("服务执行顺序错误", events)
			}

			// 2. 转换实际服务返回值，失败时的空结果继续被 DTO 拒绝。
			data, dataError := NewRevokeSessionData(result)
			if testCase.wantError != nil {
				if !errors.Is(serviceError, testCase.wantError) || result != nil || data != nil || !errors.Is(dataError, domain.ErrInvalidSessionData) {
					t.Fatal("服务失败被转换为成功数据", serviceError, dataError)
				}
				return
			}
			if serviceError != nil || dataError != nil || result == nil || data == nil || !data.Revoked {
				t.Fatal("已提交的服务结果转换失败", serviceError, dataError)
			}
		})
	}
}
