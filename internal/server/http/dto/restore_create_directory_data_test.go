package dto

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func savedCreateDirectoryFixture(t *testing.T) ([]byte, *CreateDirectoryData) {
	t.Helper()
	color := "green"
	data, err := NewCreateDirectoryData(createdDirectoryDataFixture(), &color)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return body, data
}

func TestRestoreCreateDirectoryDataRoundTrip(t *testing.T) {
	empty, green, padded := "", "green", " Sand-2_ "
	for _, color := range []*string{nil, &empty, &green, &padded} {
		entry := createdDirectoryDataFixture()
		parentID := ^uint64(0) - 1
		entry.ID, entry.ParentID, entry.Name = ^uint64(0), &parentID, "Cafe\u0301 🌱"
		data, err := NewCreateDirectoryData(entry, color)
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		for _, formatted := range []bool{false, true} {
			encoded := body
			if formatted {
				var fields map[string]any
				if err := json.Unmarshal(body, &fields); err != nil {
					t.Fatal(err)
				}
				encoded, err = json.MarshalIndent(fields, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				encoded = append([]byte(" \n"), append(encoded, '\n')...)
			}
			restored, err := RestoreCreateDirectoryData(encoded)
			if err != nil || !reflect.DeepEqual(data, restored) {
				t.Fatal("保存结果在字段排序或排版变化后未准确恢复", err)
			}
		}
	}
}

func TestRestoreCreateDirectoryDataStructure(t *testing.T) {
	body, _ := savedCreateDirectoryFixture(t)
	for _, invalid := range [][]byte{
		nil, {}, []byte("null"), []byte("[]"), []byte("42"), []byte(`"text"`), []byte("{}"),
		[]byte(`{"entry":null}`), []byte(`{"entry":[]}`), []byte(`{"entry":{}}`), []byte(`{"entry":"private"}`),
		append(append([]byte(nil), body...), []byte(" {}")...), body[:len(body)-1],
		bytes.Replace(body, []byte("学习资料"), []byte{0xff}, 1),
		bytes.Replace(body, []byte(`"entry"`), []byte(`"Entry"`), 1),
		bytes.Replace(body, []byte(`{"entry":`), []byte(`{"request_id":"private","entry":`), 1),
		bytes.Replace(body, []byte(`{"entry":`), []byte(`{"data":`), 1),
	} {
		if data, err := RestoreCreateDirectoryData(invalid); data != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) || err.Error() != "幂等记录数据异常" {
			t.Fatal("异常 JSON 未返回空结果及固定错误", err)
		}
	}
	var payload map[string]map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	for name := range payload["entry"] {
		for _, variant := range []string{"missing", "null", "wrong type", "wrong case", "unknown field"} {
			if name == "color_key" && variant == "null" {
				continue
			}
			t.Run(name+"/"+variant, func(t *testing.T) {
				var modified map[string]map[string]json.RawMessage
				if err := json.Unmarshal(body, &modified); err != nil {
					t.Fatal(err)
				}
				fields := modified["entry"]
				switch variant {
				case "missing":
					delete(fields, name)
				case "null":
					fields[name] = json.RawMessage(" \n null ")
				case "wrong type":
					fields[name] = json.RawMessage("false")
				case "wrong case":
					fields[strings.ToUpper(name)] = fields[name]
					delete(fields, name)
				case "unknown field":
					fields["private_"+name] = fields[name]
				}
				encoded, err := json.Marshal(modified)
				if err != nil {
					t.Fatal(err)
				}
				if data, err := RestoreCreateDirectoryData(encoded); data != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
					t.Fatal("必需字段、空值、类型或精确字段名未校验", err)
				}
			})
		}
	}
}

func TestRestoreCreateDirectoryDataValues(t *testing.T) {
	body, _ := savedCreateDirectoryFixture(t)
	cases := []struct {
		field string
		value any
	}{
		{"id", "0"}, {"id", "042"}, {"id", "+42"}, {"id", "18446744073709551616"}, {"id", 42},
		{"parent_id", "0"}, {"parent_id", "011"}, {"parent_id", "-1"}, {"parent_id", "42"}, {"parent_id", "18446744073709551616"}, {"parent_id", 11},
		{"name", ""}, {"entry_type", "FILE"}, {"entry_type", "dir"},
		{"size_bytes", 1}, {"size_bytes", -1}, {"size_bytes", 0.5}, {"size_bytes", "0"},
		{"revision", "0"}, {"revision", "2"}, {"revision", "01"}, {"revision", 1},
		{"status", "TRASHED"}, {"status", "PURGING"}, {"status", "PURGED"},
		{"color_key", strings.Repeat("a", 17)}, {"color_key", "绿色"}, {"color_key", "private\n"},
	}
	for _, field := range []string{"created_at", "modified_at", "updated_at"} {
		for _, timestamp := range []string{
			"", "private-time", "0001-01-01T00:00:00Z", "0999-12-31T23:59:59Z",
			"10000-01-01T00:00:00Z", "2026-02-30T00:00:00Z",
			"1000-01-01T00:30:00+01:00", "9999-12-31T23:30:00-01:00",
		} {
			cases = append(cases, struct {
				field string
				value any
			}{field, timestamp})
		}
	}
	for _, testCase := range cases {
		t.Run(testCase.field+"/"+stringMustJSON(t, testCase.value), func(t *testing.T) {
			var payload map[string]map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			payload["entry"][testCase.field] = testCase.value
			encoded := []byte(stringMustJSON(t, payload))
			if data, err := RestoreCreateDirectoryData(encoded); data != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
				t.Fatal("异常创建字段未拒绝", err)
			}
		})
	}
}

func stringMustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestRestoreCreateDirectoryDataTimes(t *testing.T) {
	body, _ := savedCreateDirectoryFixture(t)
	for _, timestamp := range []string{
		"1000-01-01T00:00:00Z", "9999-12-31T23:59:59.999999Z",
		"2026-11-16T09:30:00.123456789+08:00", "1000-01-01T01:00:00+01:00",
	} {
		var payload map[string]map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"created_at", "modified_at", "updated_at"} {
			payload["entry"][field] = timestamp
		}
		data, err := RestoreCreateDirectoryData([]byte(stringMustJSON(t, payload)))
		if err != nil {
			t.Fatal("合法时间边界未恢复", err)
		}
		expected, err := time.Parse(time.RFC3339Nano, timestamp)
		if err != nil {
			t.Fatal(err)
		}
		for _, value := range []time.Time{data.Entry.CreatedAt, data.Entry.ModifiedAt, data.Entry.UpdatedAt} {
			if !value.Equal(expected.UTC().Truncate(time.Microsecond)) || value.Location() != time.UTC || value.Nanosecond()%1000 != 0 {
				t.Fatal("恢复时间未规范为 UTC 微秒")
			}
		}
	}
}

func TestRestoreCreateDirectoryDataIndependent(t *testing.T) {
	body, expected := savedCreateDirectoryFixture(t)
	original := append([]byte(nil), body...)
	first, err := RestoreCreateDirectoryData(body)
	if err != nil || !bytes.Equal(body, original) {
		t.Fatal("恢复失败或修改了输入", err)
	}
	second, err := RestoreCreateDirectoryData(body)
	if err != nil {
		t.Fatal(err)
	}
	for index := range body {
		body[index] = 'x'
	}
	first.Entry.Name, *first.Entry.ColorKey = "改动", "sand"
	if !reflect.DeepEqual(second, expected) {
		t.Fatal("恢复结果共享了输入或可变颜色")
	}
	var workers sync.WaitGroup
	for worker := 0; worker < 32; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for attempt := 0; attempt < 20; attempt++ {
				data, err := RestoreCreateDirectoryData(original)
				if err != nil || !reflect.DeepEqual(data, expected) {
					t.Error("并发恢复结果错误", err)
					return
				}
			}
		}()
	}
	workers.Wait()
}
