package domain

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestChangeEventInputIsValid(t *testing.T) {
	valid := ChangeEventInput{EntryID: math.MaxUint64, Revision: math.MaxUint64, EventType: "ENTRY_CREATED", Payload: json.RawMessage(`{}`)}
	for _, testCase := range []struct {
		name   string
		change func(*ChangeEventInput)
		want   bool
	}{
		{name: "maximum identifiers", want: true},
		{name: "longest type and object with whitespace", change: func(input *ChangeEventInput) {
			input.EventType = strings.Repeat("E", 32)
			input.Payload = json.RawMessage(" \r\n{\"name\":\"合成目录\"}\t")
		}, want: true},
		{name: "zero entry", change: func(input *ChangeEventInput) { input.EntryID = 0 }},
		{name: "zero revision", change: func(input *ChangeEventInput) { input.Revision = 0 }},
		{name: "empty type", change: func(input *ChangeEventInput) { input.EventType = "" }},
		{name: "oversized type", change: func(input *ChangeEventInput) { input.EventType = strings.Repeat("E", 33) }},
		{name: "type with space", change: func(input *ChangeEventInput) { input.EventType = "ENTRY CREATED" }},
		{name: "non ASCII type", change: func(input *ChangeEventInput) { input.EventType = "目录" }},
		{name: "missing payload", change: func(input *ChangeEventInput) { input.Payload = nil }},
		{name: "null payload", change: func(input *ChangeEventInput) { input.Payload = json.RawMessage("null") }},
		{name: "array payload", change: func(input *ChangeEventInput) { input.Payload = json.RawMessage("[]") }},
		{name: "incomplete JSON", change: func(input *ChangeEventInput) { input.Payload = json.RawMessage("{") }},
		{name: "invalid UTF-8", change: func(input *ChangeEventInput) { input.Payload = json.RawMessage("{\"name\":\"\xff\"}") }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			input := valid
			if testCase.change != nil {
				testCase.change(&input)
			}
			if result := input.IsValid(); result != testCase.want {
				t.Fatalf("事件格式校验返回 %t，期望 %t", result, testCase.want)
			}
		})
	}
}
