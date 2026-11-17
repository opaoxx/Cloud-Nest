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

func savedCreateUploadFixture(t *testing.T) ([]byte, *CreateUploadData) {
	t.Helper()
	task := uploadTaskDataFixture()
	data, err := NewCreateUploadData(&task)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return body, data
}

func TestRestoreCreateUploadDataRoundTrip(t *testing.T) {
	for _, mode := range []string{"ordinary", "zero bytes", "single limit", "maximum IDs", "Unicode display", "historical expiry"} {
		t.Run(mode, func(t *testing.T) {
			task := uploadTaskDataFixture()
			switch mode {
			case "zero bytes":
				task.FileSize, task.ReservedBytes = 0, 0
			case "single limit":
				task.FileSize, task.ReservedBytes = domain.MaxSingleUploadSizeBytes, domain.MaxSingleUploadSizeBytes
			case "maximum IDs":
				task.ID, task.TargetParentID = ^uint64(0), ^uint64(0)
			case "Unicode display":
				task.RequestedName = "Cafe\u0301 🌱.MD"
			case "historical expiry":
				task.CreatedAt = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
				task.UpdatedAt = task.CreatedAt
				task.ExpiresAt = task.CreatedAt.Add(24 * time.Hour)
			}
			data, err := NewCreateUploadData(&task)
			if err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]json.RawMessage
			if json.Unmarshal(body, &payload) != nil {
				t.Fatal("夹具解析失败")
			}
			formatted, err := json.MarshalIndent(payload, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			formatted = append([]byte(" \n"), append(formatted, '\n')...)
			for _, encoded := range [][]byte{body, formatted} {
				restored, err := RestoreCreateUploadData(encoded)
				if err != nil || !reflect.DeepEqual(data, restored) {
					t.Fatal("创建快照在排版/字段顺序变化后未准确恢复", err)
				}
			}
		})
	}
}

func TestRestoreCreateUploadDataStructure(t *testing.T) {
	body, _ := savedCreateUploadFixture(t)
	for _, invalid := range [][]byte{
		nil, {}, []byte("null"), []byte("[]"), []byte("42"), []byte(`"private"`), []byte("{}"),
		[]byte(`{"data":{}}`), append(append([]byte(nil), body...), []byte(" {}")...), body[:len(body)-1],
		bytes.Replace(body, []byte("学习笔记"), []byte{0xff}, 1),
	} {
		data, err := RestoreCreateUploadData(invalid)
		if data != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) || err.Error() != "幂等记录数据异常" {
			t.Fatal("异常正文应返回固定错误及空结果", err)
		}
	}
	var outer map[string]json.RawMessage
	if json.Unmarshal(body, &outer) != nil {
		t.Fatal("夹具外层解析失败")
	}
	var task map[string]json.RawMessage
	if json.Unmarshal(outer["task"], &task) != nil {
		t.Fatal("夹具任务解析失败")
	}
	for _, level := range []string{"outer", "task"} {
		originals := outer
		if level == "task" {
			originals = task
		}
		for field := range originals {
			for _, variant := range []string{"missing", "null", "wrong type", "wrong case", "unknown field"} {
				if level == "task" && variant == "null" && (field == "resolved_name" || field == "result_entry_id" || field == "completed_at" || field == "last_error") {
					continue
				}
				t.Run(level+"/"+field+"/"+variant, func(t *testing.T) {
					var payload map[string]json.RawMessage
					if json.Unmarshal(body, &payload) != nil {
						t.Fatal("夹具解析失败")
					}
					fields := payload
					if level == "task" {
						fields = make(map[string]json.RawMessage)
						if json.Unmarshal(payload["task"], &fields) != nil {
							t.Fatal("任务夹具解析失败")
						}
						if len(fields) != 13 {
							t.Fatal("任务变异夹具应只包含十三个字段")
						}
					}
					switch variant {
					case "missing":
						delete(fields, field)
					case "null":
						fields[field] = json.RawMessage(" \n null ")
					case "wrong type":
						fields[field] = json.RawMessage("false")
					case "wrong case":
						fields[strings.ToUpper(field)] = fields[field]
						delete(fields, field)
					case "unknown field":
						fields["private_"+field] = fields[field]
					}
					if level == "task" {
						encoded, err := json.Marshal(fields)
						if err != nil {
							t.Fatal(err)
						}
						payload["task"] = encoded
					}
					encoded, err := json.Marshal(payload)
					if err != nil {
						t.Fatal(err)
					}
					data, err := RestoreCreateUploadData(encoded)
					if data != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) {
						t.Fatal("异常字段形状未拒绝", err)
					}
				})
			}
		}
	}
}

func TestRestoreCreateUploadDataValues(t *testing.T) {
	body, _ := savedCreateUploadFixture(t)
	type fieldChange struct{ level, field, value string }
	cases := []struct {
		name   string
		change fieldChange
	}{
		{"empty display", fieldChange{"task", "requested_name", `""`}},
		{"outer mode", fieldChange{"outer", "upload_mode", `"MULTIPART"`}},
		{"task mode", fieldChange{"task", "upload_mode", `"MULTIPART"`}},
		{"next status", fieldChange{"task", "status", `"VERIFYING"`}},
		{"oversize", fieldChange{"task", "size_bytes", "16777217"}},
		{"negative size", fieldChange{"task", "size_bytes", "-1"}},
		{"fraction size", fieldChange{"task", "size_bytes", "1.0"}},
		{"exponent size", fieldChange{"task", "size_bytes", "1e0"}},
		{"quoted size", fieldChange{"task", "size_bytes", `"0"`}},
		{"wrong task path", fieldChange{"outer", "content_path", `"/api/v1/uploads/20/content"`}},
		{"query path", fieldChange{"outer", "content_path", `"/api/v1/uploads/19/content?token=private"`}},
		{"absolute URL", fieldChange{"outer", "content_path", `"https://private.example/api/v1/uploads/19/content"`}},
		{"encoded ID", fieldChange{"outer", "content_path", `"/api/v1/uploads/%31%39/content"`}},
		{"padded path ID", fieldChange{"outer", "content_path", `"/api/v1/uploads/019/content"`}},
		{"non-null resolved name", fieldChange{"task", "resolved_name", `"result"`}},
		{"non-null entry ID", fieldChange{"task", "result_entry_id", `"31"`}},
		{"non-null completed time", fieldChange{"task", "completed_at", `"2026-11-18T02:30:00Z"`}},
		{"non-null error", fieldChange{"task", "last_error", `{"code":"INTERNAL_ERROR","message":"private"}`}},
		{"wrong expiry", fieldChange{"outer", "expires_at", `"2026-11-18T02:30:01.123456Z"`}},
		{"submicrosecond expiry mismatch", fieldChange{"outer", "expires_at", `"2026-11-18T02:30:00.123456001Z"`}},
		{"invalid created time", fieldChange{"task", "created_at", `"private-date"`}},
		{"zero updated time", fieldChange{"task", "updated_at", `"0001-01-01T00:00:00Z"`}},
		{"year below", fieldChange{"task", "created_at", `"0999-12-31T23:59:59Z"`}},
		{"year above", fieldChange{"task", "updated_at", `"10000-01-01T00:00:00Z"`}},
	}
	for _, field := range []string{"id", "parent_id"} {
		for _, value := range []string{"0", "019", "+19", "-1", "19 ", "18446744073709551616", ""} {
			quoted, _ := json.Marshal(value)
			cases = append(cases, struct {
				name   string
				change fieldChange
			}{field + "=" + value, fieldChange{"task", field, string(quoted)}})
		}
	}
	for _, fixture := range cases {
		t.Run(fixture.name, func(t *testing.T) {
			var payload map[string]json.RawMessage
			if json.Unmarshal(body, &payload) != nil {
				t.Fatal("夹具解析失败")
			}
			fields := payload
			if fixture.change.level == "task" {
				fields = make(map[string]json.RawMessage)
				if json.Unmarshal(payload["task"], &fields) != nil {
					t.Fatal("任务夹具解析失败")
				}
				if len(fields) != 13 {
					t.Fatal("任务变异夹具应只包含十三个字段")
				}
			}
			fields[fixture.change.field] = json.RawMessage(fixture.change.value)
			if fixture.change.level == "task" {
				encoded, err := json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				payload["task"] = encoded
			}
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			data, err := RestoreCreateUploadData(encoded)
			if data != nil || !errors.Is(err, domain.ErrInvalidIdempotencyRecordData) || err.Error() != "幂等记录数据异常" {
				t.Fatal("异常值未返回固定错误", err)
			}
		})
	}
}

func TestRestoreCreateUploadDataTimesAndCopies(t *testing.T) {
	for _, mode := range []string{"equivalent zones", "nanoseconds", "year bounds"} {
		t.Run(mode, func(t *testing.T) {
			body, _ := savedCreateUploadFixture(t)
			var payload map[string]json.RawMessage
			if json.Unmarshal(body, &payload) != nil {
				t.Fatal("夹具解析失败")
			}
			var task map[string]json.RawMessage
			if json.Unmarshal(payload["task"], &task) != nil {
				t.Fatal("任务夹具解析失败")
			}
			outerExpiry, innerExpiry := `"2026-11-18T10:30:00.123456+08:00"`, `"2026-11-18T02:30:00.123456Z"`
			switch mode {
			case "nanoseconds":
				outerExpiry, innerExpiry = `"2026-11-18T10:30:00.123456789+08:00"`, `"2026-11-18T02:30:00.123456789Z"`
			case "year bounds":
				task["created_at"] = json.RawMessage(`"1000-01-01T00:00:00Z"`)
				task["updated_at"] = task["created_at"]
				outerExpiry, innerExpiry = `"9999-12-31T23:59:59.999999999Z"`, `"9999-12-31T23:59:59.999999999Z"`
			}
			payload["expires_at"], task["expires_at"] = json.RawMessage(outerExpiry), json.RawMessage(innerExpiry)
			encodedTask, err := json.Marshal(task)
			if err != nil {
				t.Fatal(err)
			}
			payload["task"] = encodedTask
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			before := bytes.Clone(encoded)
			first, err := RestoreCreateUploadData(encoded)
			if err != nil || !bytes.Equal(encoded, before) || first.ExpiresAt != first.Task.ExpiresAt || first.ExpiresAt.Location() != time.UTC || first.ExpiresAt.Nanosecond()%1000 != 0 {
				t.Fatal("时间归一化、相等判断或输入保护错误", err)
			}
			second, err := RestoreCreateUploadData(encoded)
			if err != nil || !reflect.DeepEqual(first, second) {
				t.Fatal("重复恢复不稳定", err)
			}
			stable, err := json.Marshal(second)
			if err != nil {
				t.Fatal(err)
			}
			for index := range encoded {
				encoded[index] = 'X'
			}
			first.Task.RequestedName = "changed"
			first.ContentPath = "/changed"
			first.ExpiresAt = first.ExpiresAt.Add(time.Hour)
			again, err := json.Marshal(second)
			if err != nil || string(again) != string(stable) {
				t.Fatal("正文或其他结果修改影响恢复快照", err)
			}
		})
	}
}

func TestRestoreCreateUploadDataConcurrent(t *testing.T) {
	body, want := savedCreateUploadFixture(t)
	var workers sync.WaitGroup
	for worker := 0; worker < 32; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for attempt := 0; attempt < 20; attempt++ {
				data, err := RestoreCreateUploadData(body)
				if err != nil || !reflect.DeepEqual(data, want) {
					t.Error("并发快照恢复不稳定", err)
				}
			}
		}()
	}
	workers.Wait()
}
