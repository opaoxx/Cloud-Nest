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

func savedMoveEntryFixture(t *testing.T) ([]byte, *MoveEntryData) {
	t.Helper()
	data, err := NewMoveEntryData(movedEntryDataFixture())
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return body, data
}

func TestRestoreMoveEntryDataRoundTrip(t *testing.T) {
	for _, mode := range []string{"document", "image", "video", "audio", "archive", "other", "folder", "null fields", "empty text", "source id only", "source name only", "maximum integers", "historical display", "revision one"} {
		t.Run(mode, func(t *testing.T) {
			result := movedEntryDataFixture()
			target := &result.Path.Entries[len(result.Path.Entries)-1]
			switch mode {
			case "document", "image", "video", "audio", "archive", "other":
				category := strings.ToUpper(mode)
				result.Display.Category = &category
			case "folder":
				target.EntryType, target.FileSize = "DIR", 0
				result.Display.Category, result.Display.Extension = nil, nil
				color := "green"
				result.Display.ColorKey = &color
			case "null fields":
				result.Display.Extension, result.Display.MIMEType, result.Display.ColorKey = nil, nil, nil
				result.Display.SourceDeviceID, result.Display.SourceDeviceName, result.Display.ContentModifiedAt = nil, nil, nil
				result.Preference.StarredAt, result.Preference.LastOpenedAt = nil, nil
			case "empty text":
				empty := ""
				result.Display.Extension, result.Display.MIMEType, result.Display.ColorKey, result.Display.SourceDeviceName = &empty, &empty, &empty, &empty
			case "source id only":
				result.Display.SourceDeviceName = nil
			case "source name only":
				result.Display.SourceDeviceID = nil
			case "maximum integers":
				maximum, parent := ^uint64(0), ^uint64(0)-1
				result.Path.Entries[1].ID = parent
				target.ID, target.ParentID, target.Revision, target.FileSize = maximum, &parent, maximum, maxJSONSafeInteger
				result.Display.ID, result.Preference.EntryID, result.Display.SourceDeviceID = maximum, maximum, &maximum
			case "historical display":
				target.Name = "  Cafe\u0301  "
			case "revision one":
				target.Revision = 1
			}
			expected, err := NewMoveEntryData(result)
			if err != nil {
				t.Fatal(err)
			}
			body := []byte(stringMustJSON(t, expected))
			data, err := RestoreMoveEntryData(body)
			if err != nil || !reflect.DeepEqual(data, expected) {
				t.Fatal("合法快照恢复错误", err)
			}
			var formatted bytes.Buffer
			if err := json.Indent(&formatted, body, "", "  "); err != nil {
				t.Fatal(err)
			}
			data, err = RestoreMoveEntryData(formatted.Bytes())
			if err != nil || !reflect.DeepEqual(data, expected) {
				t.Fatal("排版空白改变了快照", err)
			}
		})
	}
}

func TestRestoreMoveEntryDataShape(t *testing.T) {
	body, _ := savedMoveEntryFixture(t)
	for _, invalid := range [][]byte{
		nil, []byte(""), []byte("null"), []byte("[]"), []byte("{}"), []byte(`{"entry":null}`), []byte(`{"entry":[]}`),
		append(append([]byte(nil), body...), []byte(" {}")...),
		bytes.Replace(body, []byte("学习笔记"), []byte{0xff}, 1),
		bytes.Replace(body, []byte(`"entry"`), []byte(`"Entry"`), 1),
		bytes.Replace(body, []byte(`{"entry":`), []byte(`{"request_id":"private","entry":`), 1),
		bytes.Replace(body, []byte(`{"entry":`), []byte(`{"data":`), 1),
	} {
		if data, err := RestoreMoveEntryData(invalid); data != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) || err.Error() != "幂等记录数据异常" {
			t.Fatal("非法 JSON 未返回固定错误", err)
		}
	}
	var payload map[string]map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	nullable := map[string]bool{"extension": true, "mime_type": true, "color_key": true, "content_modified_at": true, "source_device": true, "starred_at": true, "last_opened_at": true}
	for name := range payload["entry"] {
		for _, variant := range []string{"missing", "null", "wrong type", "wrong case", "unknown field"} {
			if variant == "null" && nullable[name] {
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
					fields[name] = json.RawMessage("[]")
				case "wrong case":
					fields[strings.ToUpper(name)] = fields[name]
					delete(fields, name)
				case "unknown field":
					fields["private_"+name] = fields[name]
				}
				if data, err := RestoreMoveEntryData([]byte(stringMustJSON(t, modified))); data != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
					t.Fatal("字段结构未校验", err)
				}
			})
		}
	}
}

func TestRestoreMoveEntryDataValues(t *testing.T) {
	body, _ := savedMoveEntryFixture(t)
	cases := []struct {
		field string
		value any
	}{
		{"id", "0"}, {"id", "023"}, {"id", "+23"}, {"id", "18446744073709551616"}, {"id", 23},
		{"parent_id", "0"}, {"parent_id", "012"}, {"parent_id", "-1"}, {"parent_id", "23"}, {"parent_id", "18446744073709551616"},
		{"revision", "0"}, {"revision", "04"}, {"revision", "18446744073709551616"}, {"revision", 4},
		{"name", ""}, {"kind", "FILE"}, {"kind", "DOCUMENT"}, {"kind", "unknown"},
		{"size_bytes", -1}, {"size_bytes", 0.5}, {"size_bytes", "2048"}, {"size_bytes", maxJSONSafeInteger + 1},
		{"kind", "folder"}, {"status", "TRASHED"}, {"status", "PURGING"}, {"status", "PURGED"},
		{"starred", false}, {"starred_at", nil},
		{"source_device", map[string]any{}}, {"source_device", map[string]any{"id": "19"}},
		{"source_device", map[string]any{"name": "设备"}}, {"source_device", map[string]any{"id": nil, "name": nil}},
		{"source_device", map[string]any{"ID": "19", "name": "设备"}},
		{"source_device", map[string]any{"id": "19", "name": "设备", "private": "extra"}},
		{"source_device", map[string]any{"id": "0", "name": "设备"}},
		{"source_device", map[string]any{"id": "019", "name": "设备"}},
		{"source_device", map[string]any{"id": "18446744073709551616", "name": "设备"}},
		{"source_device", map[string]any{"id": 19, "name": "设备"}},
		{"source_device", map[string]any{"id": "19", "name": 1}},
	}
	for _, field := range []string{"created_at", "modified_at", "updated_at", "content_modified_at", "starred_at", "last_opened_at"} {
		for _, stamp := range []string{"", "private-time", "0001-01-01T00:00:00Z", "0999-12-31T23:59:59Z", "10000-01-01T00:00:00Z", "2026-02-30T00:00:00Z", "1000-01-01T00:30:00+01:00", "9999-12-31T23:30:00-01:00"} {
			cases = append(cases, struct {
				field string
				value any
			}{field, stamp})
		}
	}
	for _, testCase := range cases {
		t.Run(testCase.field+"/"+stringMustJSON(t, testCase.value), func(t *testing.T) {
			var payload map[string]map[string]any
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			payload["entry"][testCase.field] = testCase.value
			if data, err := RestoreMoveEntryData([]byte(stringMustJSON(t, payload))); data != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) || err.Error() != "幂等记录数据异常" {
				t.Fatal("异常快照值未拒绝", err)
			}
		})
	}
}

func TestRestoreMoveEntryDataTimesAndCopies(t *testing.T) {
	body, expected := savedMoveEntryFixture(t)
	for _, stamp := range []string{"1000-01-01T00:00:00Z", "9999-12-31T23:59:59.999999Z", "2026-11-16T09:30:00.123456789+08:00", "1000-01-01T01:00:00+01:00"} {
		var payload map[string]map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"created_at", "modified_at", "updated_at", "content_modified_at", "starred_at", "last_opened_at"} {
			payload["entry"][field] = stamp
		}
		data, err := RestoreMoveEntryData([]byte(stringMustJSON(t, payload)))
		if err != nil {
			t.Fatal("合法时间未恢复", err)
		}
		parsed, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			t.Fatal(err)
		}
		for _, actual := range []*time.Time{&data.Entry.CreatedAt, &data.Entry.ModifiedAt, &data.Entry.UpdatedAt, data.Entry.ContentModifiedAt, data.Entry.StarredAt, data.Entry.LastOpenedAt} {
			if actual == nil || actual.Location() != time.UTC || !actual.Equal(parsed.UTC().Truncate(time.Microsecond)) {
				t.Fatal("时间未规范化")
			}
		}
	}
	original := bytes.Clone(body)
	first, err := RestoreMoveEntryData(body)
	if err != nil || !bytes.Equal(original, body) {
		t.Fatal("恢复失败或修改了输入", err)
	}
	second, err := RestoreMoveEntryData(body)
	if err != nil {
		t.Fatal(err)
	}
	for index := range body {
		body[index] = 'x'
	}
	first.Entry.Name = "later name"
	for _, text := range []*string{first.Entry.ParentID, first.Entry.Extension, first.Entry.MIMEType, first.Entry.SourceDevice.ID, first.Entry.SourceDevice.Name} {
		*text = "later text"
	}
	*first.Entry.ContentModifiedAt, *first.Entry.StarredAt, *first.Entry.LastOpenedAt = time.Now(), time.Now(), time.Now()
	if !reflect.DeepEqual(second, expected) {
		t.Fatal("恢复结果未独立持有字段")
	}
	var workers sync.WaitGroup
	for worker := 0; worker < 32; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for attempt := 0; attempt < 20; attempt++ {
				data, err := RestoreMoveEntryData(original)
				if err != nil || !reflect.DeepEqual(data, expected) {
					t.Error("并发恢复结果错误", err)
					return
				}
			}
		}()
	}
	workers.Wait()
}
