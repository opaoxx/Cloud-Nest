package dto

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestRenameDeviceRequestValidate(t *testing.T) {
	for _, testCase := range []struct {
		label, name string
		valid       bool
	}{
		{label: "ascii", name: "Study PC", valid: true},
		{label: "chinese", name: "学习电脑", valid: true},
		{label: "emoji", name: "游戏电脑 🎮", valid: true},
		{label: "minimum", name: "机", valid: true},
		{label: "maximum ascii", name: strings.Repeat("a", 100), valid: true},
		{label: "maximum chinese", name: strings.Repeat("机", 100), valid: true},
		{label: "maximum emoji", name: strings.Repeat("🎮", 100), valid: true},
		{label: "padded name", name: "  学习电脑  ", valid: true},
		{label: "combining characters", name: "Cafe\u0301", valid: true},
		{label: "empty"},
		{label: "ascii whitespace", name: "   "},
		{label: "unicode whitespace", name: " \u00a0\u3000 "},
		{label: "over ascii limit", name: strings.Repeat("a", 101)},
		{label: "over chinese limit", name: strings.Repeat("机", 101)},
		{label: "over emoji limit", name: strings.Repeat("🎮", 101)},
		{label: "combining code point limit", name: strings.Repeat("e\u0301", 51)},
		{label: "newline", name: "private-name\n"},
		{label: "tab", name: "private-name\t"},
		{label: "null byte", name: "private-name\x00"},
		{label: "delete control", name: "private-name\x7f"},
		{label: "c1 control", name: "private-name\u0085"},
		{label: "invalid utf8", name: "private-name\xff"},
	} {
		t.Run(testCase.label, func(t *testing.T) {
			// 1. 校验修改名称请求，核对输入原值及固定错误。
			request := RenameDeviceRequest{Name: testCase.name}
			before := request
			validationError := request.Validate()
			if request != before || (validationError == nil) != testCase.valid {
				t.Fatal("名称有效性或原值保护错误")
			}
			if validationError != nil && (validationError.Field != "name" || validationError.Message != "设备名称须为 1～100 个字符，不能全为空白或包含控制字符") {
				t.Fatal("设备名称字段或固定提示错误")
			}

			// 2. 相同名称经过登录校验时保持相同规则及原有字段提示。
			login := LoginRequest{Username: "root", Password: "root", InstallationID: "28ee758a-0727-42de-ae7d-3bb33155e148", DeviceName: testCase.name, Platform: "WEB", ClientType: "web"}
			loginBefore := login
			loginError := login.Validate()
			if login != loginBefore || (loginError == nil) != testCase.valid {
				t.Fatal("登录和修改名称的规则或原值不一致")
			}
			if loginError != nil && (loginError.Field != "device_name" || loginError.Message != validationError.Message) {
				t.Fatal("登录设备名称的字段或提示发生变化")
			}
		})
	}
}

func TestRenameDeviceRequestControlCharacters(t *testing.T) {
	// 1. 将 C0、DEL 和 C1 控制字符逐个嵌入正常名称，验证完整拒绝范围。
	for character := rune(0); character <= 0x9f; character++ {
		if character >= 0x20 && character < 0x7f {
			continue
		}
		t.Run(fmt.Sprintf("U+%04X", character), func(t *testing.T) {
			request := RenameDeviceRequest{Name: "学习" + string(character) + "电脑"}
			if validationError := request.Validate(); validationError == nil || validationError.Field != "name" {
				t.Fatal("控制字符未被名称校验拒绝")
			}
		})
	}
}

func TestRenameDeviceRequestJSON(t *testing.T) {
	// 1. 请求正文准确使用 name，保留空白、大小写和组合字符。
	request := RenameDeviceRequest{Name: "  Cafe\u0301 🎮  "}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal("修改名称请求编码失败", err)
	}
	var fields map[string]any
	if json.Unmarshal(encoded, &fields) != nil || len(fields) != 1 || fields["name"] != request.Name {
		t.Fatal("请求字段或名称原文错误")
	}
	var decoded RenameDeviceRequest
	if json.Unmarshal(encoded, &decoded) != nil || decoded != request || decoded.Validate() != nil {
		t.Fatal("名称请求往返编码或校验失败")
	}
}
