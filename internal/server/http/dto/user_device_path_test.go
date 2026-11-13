package dto

import (
	"strconv"
	"strings"
	"testing"
)

func TestParseUserDevicePath(t *testing.T) {
	for _, testCase := range []struct {
		name, raw string
		deviceID  uint64
	}{
		{name: "smallest id", raw: "1", deviceID: 1},
		{name: "ordinary id", raw: "13", deviceID: 13},
		{name: "above javascript safe integer", raw: "9007199254740993", deviceID: 9007199254740993},
		{name: "above signed integer", raw: "9223372036854775808", deviceID: 9223372036854775808},
		{name: "full unsigned id", raw: "18446744073709551615", deviceID: ^uint64(0)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			input, fieldError := ParseUserDevicePath(testCase.raw)
			if fieldError != nil || input == nil || input.DeviceID != testCase.deviceID || strconv.FormatUint(input.DeviceID, 10) != testCase.raw {
				t.Fatal("合法设备 ID 转换不准确", fieldError)
			}
		})
	}
	first, _ := ParseUserDevicePath("13")
	second, _ := ParseUserDevicePath("13")
	first.DeviceID = 17
	if first == second || second.DeviceID != 13 {
		t.Fatal("两次解析共享了可修改结果")
	}
}

func TestParseUserDevicePathFailures(t *testing.T) {
	for _, testCase := range []struct {
		name, raw string
	}{
		{name: "missing id"},
		{name: "zero", raw: "0"},
		{name: "padded zero", raw: "00"},
		{name: "leading zero", raw: "013"},
		{name: "plus sign", raw: "+13"},
		{name: "negative id", raw: "-13"},
		{name: "leading space", raw: " 13"},
		{name: "trailing space", raw: "13 "},
		{name: "tab", raw: "\t13"},
		{name: "newline", raw: "13\n"},
		{name: "decimal", raw: "13.0"},
		{name: "exponent", raw: "1e2"},
		{name: "hex", raw: "0x13"},
		{name: "underscore", raw: "1_3"},
		{name: "comma", raw: "1,3"},
		{name: "full width digits", raw: "１３"},
		{name: "arabic digits", raw: "١٣"},
		{name: "overflow", raw: "18446744073709551616"},
		{name: "very long id", raw: strings.Repeat("9", 4096)},
		{name: "encoded digit remains", raw: "%31%33"},
		{name: "encoded sign remains", raw: "%2B13"},
		{name: "multiple ids", raw: "13/17"},
		{name: "query text", raw: "13?id=17"},
		{name: "fragment text", raw: "13#17"},
		{name: "nul byte", raw: "13\x00"},
		{name: "invalid utf8", raw: "13\xff"},
		{name: "private input", raw: "private-device-value"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			input, fieldError := ParseUserDevicePath(testCase.raw)
			if input != nil || fieldError == nil || fieldError.Field != "id" || fieldError.Message != "设备 ID 须为 1～18446744073709551615 的标准十进制整数" {
				t.Fatal("错误输入未返回空结果及固定字段错误")
			}
		})
	}
	_, firstError := ParseUserDevicePath("0")
	_, secondError := ParseUserDevicePath("0")
	firstError.Field, firstError.Message = "changed", "changed"
	if firstError == secondError || secondError.Field != "id" || secondError.Message != "设备 ID 须为 1～18446744073709551615 的标准十进制整数" {
		t.Fatal("错误对象在调用之间共享可修改状态")
	}
}
