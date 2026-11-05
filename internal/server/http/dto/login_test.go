package dto

import (
	"strings"
	"testing"
)

func TestLoginRequestValidate(t *testing.T) {
	// 1. 从合法请求复制，各场景只改变一个规则；原始数据必须保持不变。
	base := LoginRequest{Username: "root", Password: "root", InstallationID: "28ee758a-0727-42de-ae7d-3bb33155e148", DeviceName: "学习用浏览器", Platform: "WEB", ClientType: "web"}
	for _, testCase := range []struct {
		name      string
		change    func(*LoginRequest)
		wantField string
	}{
		{name: "web root"},
		{name: "windows native", change: func(request *LoginRequest) { request.Platform, request.ClientType = "WINDOWS", "native" }},
		{name: "android native", change: func(request *LoginRequest) { request.Platform, request.ClientType = "ANDROID", "native" }},
		{name: "username minimum", change: func(request *LoginRequest) { request.Username = "a" }},
		{name: "username maximum", change: func(request *LoginRequest) { request.Username = strings.Repeat("a", 64) }},
		{name: "username permitted alphabet", change: func(request *LoginRequest) { request.Username = "student_2026" }},
		{name: "username empty", change: func(request *LoginRequest) { request.Username = "" }, wantField: "username"},
		{name: "username too long", change: func(request *LoginRequest) { request.Username = strings.Repeat("a", 65) }, wantField: "username"},
		{name: "username uppercase", change: func(request *LoginRequest) { request.Username = "ROOT" }, wantField: "username"},
		{name: "username leading space", change: func(request *LoginRequest) { request.Username = " root" }, wantField: "username"},
		{name: "username trailing space", change: func(request *LoginRequest) { request.Username = "root " }, wantField: "username"},
		{name: "username non ascii", change: func(request *LoginRequest) { request.Username = "用户" }, wantField: "username"},
		{name: "username punctuation", change: func(request *LoginRequest) { request.Username = "private-user" }, wantField: "username"},
		{name: "password minimum", change: func(request *LoginRequest) { request.Password = "a" }},
		{name: "password maximum bytes", change: func(request *LoginRequest) { request.Password = strings.Repeat("a", 72) }},
		{name: "password exact utf8 bytes", change: func(request *LoginRequest) { request.Password = strings.Repeat("中", 24) }},
		{name: "password spaces preserved", change: func(request *LoginRequest) { request.Password = " private-password " }},
		{name: "password only spaces preserved", change: func(request *LoginRequest) { request.Password = " " }},
		{name: "password empty", change: func(request *LoginRequest) { request.Password = "" }, wantField: "password"},
		{name: "password too long", change: func(request *LoginRequest) { request.Password = strings.Repeat("a", 73) }, wantField: "password"},
		{name: "password utf8 over byte limit", change: func(request *LoginRequest) { request.Password = strings.Repeat("中", 25) }, wantField: "password"},
		{name: "password invalid utf8", change: func(request *LoginRequest) { request.Password = "\xff" }, wantField: "password"},
		{name: "installation missing", change: func(request *LoginRequest) { request.InstallationID = "" }, wantField: "installation_id"},
		{name: "installation invalid", change: func(request *LoginRequest) { request.InstallationID = "private-installation" }, wantField: "installation_id"},
		{name: "installation uppercase", change: func(request *LoginRequest) { request.InstallationID = strings.ToUpper(base.InstallationID) }, wantField: "installation_id"},
		{name: "installation nil uuid", change: func(request *LoginRequest) { request.InstallationID = "00000000-0000-0000-0000-000000000000" }, wantField: "installation_id"},
		{name: "installation wrong version", change: func(request *LoginRequest) { request.InstallationID = "28ee758a-0727-12de-ae7d-3bb33155e148" }, wantField: "installation_id"},
		{name: "installation wrong variant", change: func(request *LoginRequest) { request.InstallationID = "28ee758a-0727-42de-0e7d-3bb33155e148" }, wantField: "installation_id"},
		{name: "installation without hyphens", change: func(request *LoginRequest) { request.InstallationID = strings.ReplaceAll(base.InstallationID, "-", "") }, wantField: "installation_id"},
		{name: "installation braces", change: func(request *LoginRequest) { request.InstallationID = "{" + base.InstallationID + "}" }, wantField: "installation_id"},
		{name: "installation urn", change: func(request *LoginRequest) { request.InstallationID = "urn:uuid:" + base.InstallationID }, wantField: "installation_id"},
		{name: "device minimum", change: func(request *LoginRequest) { request.DeviceName = "机" }},
		{name: "device maximum unicode", change: func(request *LoginRequest) { request.DeviceName = strings.Repeat("机", 100) }},
		{name: "device maximum emoji", change: func(request *LoginRequest) { request.DeviceName = strings.Repeat("🎮", 100) }},
		{name: "device spaces preserved", change: func(request *LoginRequest) { request.DeviceName = "  我的电脑  " }},
		{name: "device empty", change: func(request *LoginRequest) { request.DeviceName = "" }, wantField: "device_name"},
		{name: "device too long", change: func(request *LoginRequest) { request.DeviceName = strings.Repeat("机", 101) }, wantField: "device_name"},
		{name: "device only unicode space", change: func(request *LoginRequest) { request.DeviceName = " \u3000 " }, wantField: "device_name"},
		{name: "device control newline", change: func(request *LoginRequest) { request.DeviceName = "private-device\n" }, wantField: "device_name"},
		{name: "device control nul", change: func(request *LoginRequest) { request.DeviceName = "private-device\x00" }, wantField: "device_name"},
		{name: "device invalid utf8", change: func(request *LoginRequest) { request.DeviceName = "\xff" }, wantField: "device_name"},
		{name: "platform missing", change: func(request *LoginRequest) { request.Platform = "" }, wantField: "platform"},
		{name: "platform unknown", change: func(request *LoginRequest) { request.Platform = "private-platform" }, wantField: "platform"},
		{name: "platform lowercase", change: func(request *LoginRequest) { request.Platform = "web" }, wantField: "platform"},
		{name: "client type missing", change: func(request *LoginRequest) { request.ClientType = "" }, wantField: "client_type"},
		{name: "client type uppercase", change: func(request *LoginRequest) { request.ClientType = "WEB" }, wantField: "client_type"},
		{name: "client type unknown", change: func(request *LoginRequest) { request.ClientType = "private-client" }, wantField: "client_type"},
		{name: "web with windows", change: func(request *LoginRequest) { request.Platform = "WINDOWS" }, wantField: "platform"},
		{name: "web with android", change: func(request *LoginRequest) { request.Platform = "ANDROID" }, wantField: "platform"},
		{name: "native with web", change: func(request *LoginRequest) { request.ClientType = "native" }, wantField: "platform"},
		{name: "first error is deterministic", change: func(request *LoginRequest) { request.Username, request.Password = "", "" }, wantField: "username"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			request := base
			if testCase.change != nil {
				testCase.change(&request)
			}
			original := request
			validationError := request.Validate()
			if request != original {
				t.Fatal("校验修改了原始请求")
			}
			if testCase.wantField == "" {
				if validationError != nil {
					t.Fatalf("合法请求被拒绝: %+v", validationError)
				}
				return
			}
			// 2. 只返回固定字段名和公开提示，不能附带实际输入。
			if validationError == nil || validationError.Field != testCase.wantField || validationError.Message == "" {
				t.Fatalf("字段错误=%+v，期望=%s", validationError, testCase.wantField)
			}
			if strings.Contains(validationError.Message, "private-") {
				t.Fatal("提示包含了原始输入")
			}
		})
	}
}
