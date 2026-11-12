package dto

import "testing"

func TestLogoutRequestValidate(t *testing.T) {
	for _, testCase := range []struct {
		name, clientType string
		valid            bool
	}{
		{name: "web", clientType: "web", valid: true},
		{name: "native", clientType: "native", valid: true},
		{name: "missing client"},
		{name: "unknown client", clientType: "private-client"},
		{name: "uppercase web", clientType: "WEB"},
		{name: "uppercase native", clientType: "NATIVE"},
		{name: "padded web", clientType: " web "},
		{name: "padded native", clientType: " native "},
		{name: "whitespace", clientType: " \t\n"},
		{name: "control suffix", clientType: "web\x00"},
		{name: "unicode lookalike", clientType: "ｗｅｂ"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request := LogoutRequest{ClientType: testCase.clientType}
			before := request
			validationError := request.Validate()
			if request != before {
				t.Fatal("校验修改了客户端类型原值")
			}
			if testCase.valid {
				if validationError != nil {
					t.Fatal("合法客户端类型被拒绝", validationError)
				}
				return
			}
			if validationError == nil || validationError.Field != "client_type" || validationError.Message != "客户端类型须为 web 或 native" {
				t.Fatal("字段名或固定提示不正确")
			}
		})
	}
}
