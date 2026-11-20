package dto

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func savedTrashEntryFixture(t *testing.T) ([]byte, *TrashEntryData) {
	t.Helper()
	data, err := NewTrashEntryData(trashEntryDataFixture())
	if err != nil {
		t.Fatal(err)
	}
	return []byte(stringMustJSON(t, data)), data
}

func TestRestoreTrashEntryDataRoundTrip(t *testing.T) {
	for _, scenario := range []string{"file", "directory with high identifiers"} {
		t.Run(scenario, func(t *testing.T) {
			result := trashEntryDataFixture()
			if scenario == "directory with high identifiers" {
				result.ID, *result.ParentID, result.Revision = math.MaxUint64, math.MaxUint64-1, math.MaxUint64
				result.EntryType, result.FileSize, result.Name = "DIR", 0, "  Cafe\u0301 🌱  "
			}
			want, err := NewTrashEntryData(result)
			if err != nil {
				t.Fatal(err)
			}
			body := []byte(stringMustJSON(t, want))

			// 1. 调整字段顺序与空白，模拟数据库 JSON 格式化。
			var fields map[string]map[string]json.RawMessage
			if err := json.Unmarshal(body, &fields); err != nil {
				t.Fatal(err)
			}
			normalized, err := json.MarshalIndent(fields, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			before := bytes.Clone(normalized)
			recovered, err := RestoreTrashEntryData(normalized)
			if err != nil || !reflect.DeepEqual(recovered, want) || !bytes.Equal(normalized, before) {
				t.Fatal("回收快照恢复失败或修改了输入", recovered, err)
			}

			// 2. 恢复结果独立于输入和同一快照的其他结果。
			second, err := RestoreTrashEntryData(normalized)
			if err != nil {
				t.Fatal(err)
			}
			for index := range normalized {
				normalized[index] = 'x'
			}
			recovered.Entry.Name = "later change"
			if !reflect.DeepEqual(second, want) {
				t.Fatal("恢复结果未独立持有数据")
			}
		})
	}
}

func TestRestoreTrashEntryDataShape(t *testing.T) {
	body, _ := savedTrashEntryFixture(t)
	assertRejected := func(t *testing.T, input []byte) {
		t.Helper()
		data, err := RestoreTrashEntryData(input)
		if data != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
			t.Fatal("异常快照结构未拒绝", data, err)
		}
	}
	for _, scenario := range []struct {
		name  string
		input []byte
	}{
		{"nil", nil},
		{"null", []byte("null")},
		{"array", []byte("[]")},
		{"missing entry", []byte("{}")},
		{"null entry", []byte(`{"entry":null}`)},
		{"array entry", []byte(`{"entry":[]}`)},
		{"wrong outer field case", bytes.Replace(body, []byte(`"entry":`), []byte(`"Entry":`), 1)},
		{"request ID in stable data", bytes.Replace(body, []byte(`{"entry":`), []byte(`{"request_id":"fixture","entry":`), 1)},
		{"duplicate entry", bytes.Replace(body, []byte(`{"entry":`), []byte(`{"entry":{},"entry":`), 1)},
		{"duplicate field", bytes.Replace(body, []byte(`"id":"23"`), []byte(`"id":"23","id":"23"`), 1)},
		{"escaped duplicate field", bytes.Replace(body, []byte(`"id":"23"`), []byte(`"id":"23","i\u0064":"23"`), 1)},
		{"trailing value", append(bytes.Clone(body), []byte(" {}")...)},
		{"missing closing brace", body[:len(body)-1]},
		{"invalid UTF-8", bytes.Replace(body, []byte("学习笔记"), []byte{0xff}, 1)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			assertRejected(t, scenario.input)
		})
	}
	for _, fieldName := range []string{"id", "parent_id", "name", "entry_type", "status", "revision", "deleted_at"} {
		for _, variant := range []string{"missing", "null", "number", "wrong case", "unknown field"} {
			t.Run(fieldName+"/"+variant, func(t *testing.T) {
				var payload map[string]map[string]json.RawMessage
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatal(err)
				}
				fields := payload["entry"]
				switch variant {
				case "missing":
					delete(fields, fieldName)
				case "null":
					fields[fieldName] = json.RawMessage("null")
				case "number":
					fields[fieldName] = json.RawMessage("1")
				case "wrong case":
					fields[strings.ToUpper(fieldName)] = fields[fieldName]
					delete(fields, fieldName)
				case "unknown field":
					fields["extra"] = fields[fieldName]
				}
				assertRejected(t, []byte(stringMustJSON(t, payload)))
			})
		}
	}
}

func TestRestoreTrashEntryDataValues(t *testing.T) {
	body, _ := savedTrashEntryFixture(t)
	for _, scenario := range []struct {
		field, value string
	}{
		{"id", "0"}, {"id", "023"}, {"id", "+23"}, {"id", "18446744073709551616"},
		{"parent_id", "0"}, {"parent_id", "-1"}, {"parent_id", "23"}, {"parent_id", "18446744073709551616"},
		{"revision", "0"}, {"revision", "04"}, {"revision", "18446744073709551616"},
		{"name", ""}, {"entry_type", "OTHER"}, {"status", "ACTIVE"}, {"status", "PURGING"},
		{"deleted_at", "0001-01-01T00:00:00Z"}, {"deleted_at", "10000-01-01T00:00:00Z"},
		{"deleted_at", "2026-02-30T00:00:00Z"}, {"deleted_at", "2026-11-20T04:30:00.1234567Z"},
		{"deleted_at", "2026-11-20T12:30:00.123456+08:00"}, {"deleted_at", "2026-11-20T04:30:00+00:00"},
		{"deleted_at", "2026-11-20T04:30:00,123456Z"}, {"deleted_at", "2026-11-20t04:30:00z"},
	} {
		t.Run(scenario.field+"/"+scenario.value, func(t *testing.T) {
			var payload map[string]map[string]string
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			payload["entry"][scenario.field] = scenario.value
			recovered, err := RestoreTrashEntryData([]byte(stringMustJSON(t, payload)))
			if recovered != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
				t.Fatal("不符合回收契约的字段未拒绝", recovered, err)
			}
		})
	}
}

func TestRestoreTrashEntryDataUTCBoundaries(t *testing.T) {
	for _, stamp := range []string{
		"1000-01-01T00:00:00Z", "9999-12-31T23:59:59.999999Z", "2026-11-20T04:30:00.123400Z",
	} {
		t.Run(stamp, func(t *testing.T) {
			body, want := savedTrashEntryFixture(t)
			adjusted := bytes.Replace(body, []byte(want.Entry.DeletedAt.Format(time.RFC3339Nano)), []byte(stamp), 1)
			wantTime, err := time.Parse(time.RFC3339Nano, stamp)
			if err != nil {
				t.Fatal(err)
			}
			want.Entry.DeletedAt = wantTime.UTC()
			data, err := RestoreTrashEntryData(adjusted)
			if err != nil || !reflect.DeepEqual(data, want) {
				t.Fatal("合法 UTC 微秒时间未恢复", data, err)
			}
		})
	}
}
