package dto

import (
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestNewCreateUploadDataJSON(t *testing.T) {
	// 1. 从已有任务夹具取得公开创建结果，并组装当前请求外层。
	source := uploadTaskDataFixture()
	data, err := NewCreateUploadData(&source)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(CreateUploadResponse{Data: *data, RequestID: "28ee758a-0727-42de-ae7d-3bb33155e148"})
	expected := `{"data":{"task":{"id":"19","parent_id":"13","requested_name":"学习笔记.MD","resolved_name":null,"size_bytes":2048,"upload_mode":"SINGLE","status":"UPLOADING","result_entry_id":null,"created_at":"2026-11-17T02:30:00.123456Z","updated_at":"2026-11-17T02:30:01.123456Z","expires_at":"2026-11-18T02:30:00.123456Z","completed_at":null,"last_error":null},"upload_mode":"SINGLE","content_path":"/api/v1/uploads/19/content","expires_at":"2026-11-18T02:30:00.123456Z"},"request_id":"28ee758a-0727-42de-ae7d-3bb33155e148"}`
	if err != nil || string(encoded) != expected {
		t.Fatal("上传创建 JSON 字段、路径或时间错误", err)
	}
	// 2. 稳定 data 与请求外层各有独立字段范围，内部事实由公开 DTO 筛选。
	stable, err := json.Marshal(data)
	var fields map[string]json.RawMessage
	if err != nil || json.Unmarshal(stable, &fields) != nil || len(fields) != 4 {
		t.Fatal("稳定创建结果字段范围错误", err)
	}
	for _, forbidden := range []string{"request_id", "user_id", "device_id", "staged_object_id", "expected_sha256", "lease_owner", "last_error_message", "private-upload"} {
		if strings.Contains(string(stable), forbidden) {
			t.Fatal("创建结果编码包含内部事实或请求外层")
		}
	}
}

func TestNewCreateUploadDataResults(t *testing.T) {
	for _, mode := range []string{"ordinary", "zero bytes", "single limit", "maximum IDs", "Unicode display", "past expiry", "year bounds"} {
		t.Run(mode, func(t *testing.T) {
			source := uploadTaskDataFixture()
			switch mode {
			case "zero bytes":
				source.FileSize, source.ReservedBytes = 0, 0
			case "single limit":
				source.FileSize, source.ReservedBytes = domain.MaxSingleUploadSizeBytes, domain.MaxSingleUploadSizeBytes
			case "maximum IDs":
				source.ID, source.UserID, source.DeviceID, source.StagedObjectID, source.TargetParentID = ^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0)
			case "Unicode display":
				source.RequestedName = "Cafe\u0301 🌱.MD"
			case "past expiry":
				source.CreatedAt = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
				source.UpdatedAt, source.ExpiresAt = source.CreatedAt, source.CreatedAt.Add(24*time.Hour)
			case "year bounds":
				source.CreatedAt = time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
				source.UpdatedAt = source.CreatedAt
				source.ExpiresAt = time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
			}
			before := source
			data, err := NewCreateUploadData(&source)
			if err != nil || data == nil || !reflect.DeepEqual(source, before) {
				t.Fatal("有效创建快照转换或来源保护错误", err)
			}
			existing, err := NewUploadTaskData(&source)
			if err != nil || !reflect.DeepEqual(data.Task, *existing) || data.UploadMode != "SINGLE" || data.ContentPath != "/api/v1/uploads/"+strconv.FormatUint(source.ID, 10)+"/content" || data.ExpiresAt != data.Task.ExpiresAt || data.ExpiresAt.Location() != time.UTC || data.ExpiresAt.Nanosecond()%1000 != 0 {
				t.Fatal("任务复用、完整路径或到期时间一致性错误", err)
			}
			if _, err := json.Marshal(data); err != nil {
				t.Fatal("有效创建结果无法编码", err)
			}
		})
	}
}

func TestNewCreateUploadDataFailures(t *testing.T) {
	for _, mode := range []string{
		"nil", "zero id", "zero user", "zero device", "zero object", "zero parent", "empty name", "invalid UTF8", "MULTIPART", "wrong status", "oversize", "maximum size", "wrong reservation", "resolved name", "result entry", "completed time", "error code", "zero created", "zero updated", "zero expiry", "year below", "year above",
	} {
		t.Run(mode, func(t *testing.T) {
			source := uploadTaskDataFixture()
			input := &source
			switch mode {
			case "nil":
				input = nil
			case "zero id":
				source.ID = 0
			case "zero user":
				source.UserID = 0
			case "zero device":
				source.DeviceID = 0
			case "zero object":
				source.StagedObjectID = 0
			case "zero parent":
				source.TargetParentID = 0
			case "empty name":
				source.RequestedName = ""
			case "invalid UTF8":
				source.RequestedName = "private\xff"
			case "MULTIPART":
				source.UploadMode = "MULTIPART"
			case "wrong status":
				source.Status = domain.UploadStatusVerifying
			case "oversize":
				source.FileSize, source.ReservedBytes = domain.MaxSingleUploadSizeBytes+1, domain.MaxSingleUploadSizeBytes+1
			case "maximum size":
				source.FileSize, source.ReservedBytes = ^uint64(0), ^uint64(0)
			case "wrong reservation":
				source.ReservedBytes++
			case "resolved name":
				source.ResolvedName = new(string)
			case "result entry":
				source.ResultEntryID = new(uint64)
			case "completed time":
				source.CompletedAt = new(time.Time)
			case "error code":
				source.LastErrorCode = new(string)
			case "zero created":
				source.CreatedAt = time.Time{}
			case "zero updated":
				source.UpdatedAt = time.Time{}
			case "zero expiry":
				source.ExpiresAt = time.Time{}
			case "year below":
				source.CreatedAt = time.Date(999, 12, 31, 0, 0, 0, 0, time.UTC)
			case "year above":
				source.ExpiresAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			}
			data, err := NewCreateUploadData(input)
			if data != nil || !errors.Is(err, domain.ErrInvalidUploadTaskData) {
				t.Fatal("非法创建快照应返回空结果和固定错误", err)
			}
		})
	}
}

func TestNewCreateUploadDataCopies(t *testing.T) {
	source := uploadTaskDataFixture()
	first, err := NewCreateUploadData(&source)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewCreateUploadData(&source)
	if err != nil {
		t.Fatal(err)
	}
	stable, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	source.RequestedName, source.Status = "changed source", domain.UploadStatusVerifying
	source.ExpiresAt = source.ExpiresAt.Add(time.Hour)
	source.ExpectedSHA256[0] ^= 0xff
	first.Task.RequestedName, first.ContentPath = "changed DTO", "/changed"
	first.ExpiresAt = first.ExpiresAt.Add(time.Hour)
	again, err := json.Marshal(second)
	if err != nil || string(again) != string(stable) {
		t.Fatal("创建快照共享了来源或其他结果的状态", err)
	}
}

func TestNewCreateUploadDataConcurrent(t *testing.T) {
	source := uploadTaskDataFixture()
	expected, err := NewCreateUploadData(&source)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for worker := 0; worker < 32; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for attempt := 0; attempt < 20; attempt++ {
				data, err := NewCreateUploadData(&source)
				if err != nil || !reflect.DeepEqual(data, expected) {
					t.Error("并发创建响应转换不稳定", err)
				}
			}
		}()
	}
	workers.Wait()
}
