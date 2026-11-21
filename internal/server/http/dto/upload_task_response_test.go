package dto

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func uploadTaskDataFixture() domain.UploadTask {
	stamp := time.Date(2026, 11, 17, 10, 30, 0, 123456789, time.FixedZone("UTC+8", 8*60*60))
	digest := sha256.Sum256([]byte("private-upload-digest"))
	leaseOwner, diagnostic := "private-upload-worker", "private-upload-diagnostic /private/object?token=fixture"
	leaseUntil := stamp.Add(time.Minute)
	return domain.UploadTask{
		ID: 19, UserID: 7, DeviceID: 11, StagedObjectID: 23, TargetParentID: 13,
		RequestedName: "学习笔记.MD", UploadMode: "SINGLE", ExpectedSHA256: &digest,
		FileSize: 2048, Status: domain.UploadStatusUploading, ReservedBytes: 2048,
		LeaseOwner: &leaseOwner, LeaseUntil: &leaseUntil, LeaseVersion: 3, AttemptCount: 2,
		LastErrorMessage: &diagnostic, CreatedAt: stamp, UpdatedAt: stamp.Add(time.Second), ExpiresAt: stamp.Add(24 * time.Hour),
	}
}

func TestNewUploadTaskDataJSON(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(strconv.FormatBool(completed), func(t *testing.T) {
			// 1. 准备上传中或已完成的任务，转换为公开数据。
			source := uploadTaskDataFixture()
			resolvedJSON, resultJSON, completedJSON, status := "null", "null", "null", "UPLOADING"
			if completed {
				name, entryID, completedAt := "学习笔记 (1).MD", ^uint64(0), source.CreatedAt.Add(3*time.Second)
				source.Status, source.ReservedBytes = domain.UploadStatusCompleted, 0
				source.ResolvedName, source.ResultEntryID, source.CompletedAt = &name, &entryID, &completedAt
				resolvedJSON, resultJSON = `"学习笔记 (1).MD"`, `"18446744073709551615"`
				completedJSON, status = `"2026-11-17T02:30:03.123456Z"`, "COMPLETED"
			}
			data, err := NewUploadTaskData(&source)
			if err != nil {
				t.Fatal(err)
			}
			// 2. 核对十三个公开字段、明确 null、精确时间与请求 ID 外层。
			encoded, err := json.Marshal(UploadTaskResponse{Data: *data, RequestID: "28ee758a-0727-42de-ae7d-3bb33155e148"})
			expected := `{"data":{"id":"19","parent_id":"13","requested_name":"学习笔记.MD","resolved_name":` + resolvedJSON +
				`,"size_bytes":2048,"upload_mode":"SINGLE","status":"` + status + `","result_entry_id":` + resultJSON +
				`,"created_at":"2026-11-17T02:30:00.123456Z","updated_at":"2026-11-17T02:30:01.123456Z","expires_at":"2026-11-18T02:30:00.123456Z","completed_at":` +
				completedJSON + `,"last_error":null},"request_id":"28ee758a-0727-42de-ae7d-3bb33155e148"}`
			if err != nil || string(encoded) != expected {
				t.Fatal("任务公开 JSON 字段、空值或编码错误", err)
			}
		})
	}
}

func TestNewUploadTaskDataResults(t *testing.T) {
	modes := []string{"zero bytes", "maximum JSON safe size", "full unsigned IDs", "unicode name", "MULTIPART", "expired retained", "time boundaries"}
	for _, status := range []string{"CREATING", "UPLOADING", "COMPLETING", "VERIFYING", "COMPLETED", "CANCELLING", "CANCELLED", "EXPIRING", "EXPIRED", "FAILED"} {
		modes = append(modes, status)
	}
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			// 1. 准备公开数值、名称、时间及状态的合法边界。
			source := uploadTaskDataFixture()
			switch mode {
			case "zero bytes":
				source.FileSize, source.ReservedBytes = 0, 0
			case "maximum JSON safe size":
				source.FileSize = 9007199254740991
			case "full unsigned IDs":
				maximum := ^uint64(0)
				source.ID, source.UserID, source.TargetParentID, source.ResultEntryID = maximum, maximum, maximum, &maximum
			case "unicode name":
				source.RequestedName = "Café 🌱.MD"
			case "MULTIPART":
				source.UploadMode = "MULTIPART"
			case "expired retained":
				source.ExpiresAt = source.CreatedAt.Add(-24 * time.Hour)
			case "time boundaries":
				source.CreatedAt = time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
				source.UpdatedAt = time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
			default:
				source.Status = domain.UploadStatus(mode)
				if source.Status.IsTerminal() {
					source.ReservedBytes = 0
				}
				if source.Status == domain.UploadStatusCompleted {
					entryID, stamp := uint64(31), source.UpdatedAt
					source.ResultEntryID, source.CompletedAt = &entryID, &stamp
				}
			}
			before := source
			data, err := NewUploadTaskData(&source)
			if err != nil || data == nil || !reflect.DeepEqual(source, before) || data.ID != strconv.FormatUint(source.ID, 10) ||
				data.ParentID != strconv.FormatUint(source.TargetParentID, 10) || data.SizeBytes != source.FileSize ||
				data.RequestedName != source.RequestedName || data.Status != string(source.Status) || data.UploadMode != source.UploadMode {
				t.Fatal("合法任务响应转换或原值保护错误", err)
			}
			// 2. UTC 微秒转换与完整 ID 可以安全编码为 JSON。
			for _, stamp := range []struct{ actual, original time.Time }{
				{data.CreatedAt, source.CreatedAt}, {data.UpdatedAt, source.UpdatedAt}, {data.ExpiresAt, source.ExpiresAt},
			} {
				if stamp.actual.Location() != time.UTC || !stamp.actual.Equal(stamp.original.UTC().Truncate(time.Microsecond)) {
					t.Fatal("任务时间规范化错误")
				}
			}
			if source.ResultEntryID != nil && (data.ResultEntryID == nil || *data.ResultEntryID != strconv.FormatUint(*source.ResultEntryID, 10)) {
				t.Fatal("结果目录项完整 ID 转换错误")
			}
			if _, err := json.Marshal(data); err != nil {
				t.Fatal("合法任务编码失败", err)
			}
		})
	}
}

func TestNewUploadTaskDataFailures(t *testing.T) {
	for _, mode := range []string{
		"nil", "zero id", "zero user", "zero parent", "unknown status", "empty name", "invalid name encoding",
		"empty resolved name", "invalid resolved encoding", "unknown mode", "zero result id",
		"completed missing result", "completed missing time", "unsafe size", "maximum unsigned size",
	} {
		t.Run(mode, func(t *testing.T) {
			// 1. 异常公开字段整体返回 nil，固定错误保留来源数据。
			source := uploadTaskDataFixture()
			result := &source
			switch mode {
			case "nil":
				result = nil
			case "zero id":
				source.ID = 0
			case "zero user":
				source.UserID = 0
			case "zero parent":
				source.TargetParentID = 0
			case "unknown status":
				source.Status = "UNKNOWN"
			case "empty name":
				source.RequestedName = ""
			case "invalid name encoding":
				source.RequestedName = "\xff"
			case "empty resolved name", "invalid resolved encoding":
				name := ""
				if mode == "invalid resolved encoding" {
					name = "\xff"
				}
				source.ResolvedName = &name
			case "unknown mode":
				source.UploadMode = "UNKNOWN"
			case "zero result id":
				source.ResultEntryID = new(uint64)
			case "completed missing result":
				stamp := source.UpdatedAt
				source.Status, source.CompletedAt = domain.UploadStatusCompleted, &stamp
			case "completed missing time":
				entryID := uint64(31)
				source.Status, source.ResultEntryID = domain.UploadStatusCompleted, &entryID
			case "unsafe size":
				source.FileSize = 9007199254740992
			case "maximum unsigned size":
				source.FileSize = ^uint64(0)
			}
			before := source
			data, err := NewUploadTaskData(result)
			if data != nil || err == nil || !reflect.DeepEqual(source, before) {
				t.Fatal("异常任务被交付或来源被修改", err)
			}
			if strings.Contains(mode, "size") {
				if err.Error() != "上传文件字节数超出 JSON 安全整数范围" {
					t.Fatal("大小错误提示不符")
				}
			} else if !errors.Is(err, domain.ErrInvalidUploadTaskData) {
				t.Fatal("任务数据异常类型错误", err)
			}
		})
	}
	// 2. 四个公开时间分别覆盖零值及数据库年份范围异常。
	for _, fieldName := range []string{"CreatedAt", "UpdatedAt", "ExpiresAt", "CompletedAt"} {
		for _, stamp := range []time.Time{
			{}, time.Date(999, 12, 31, 23, 59, 59, 0, time.UTC), time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC),
		} {
			t.Run(fmt.Sprintf("%s year %d", fieldName, stamp.Year()), func(t *testing.T) {
				source := uploadTaskDataFixture()
				field := reflect.ValueOf(&source).Elem().FieldByName(fieldName)
				if field.Kind() == reflect.Pointer {
					field.Set(reflect.ValueOf(&stamp))
				} else {
					field.Set(reflect.ValueOf(stamp))
				}
				data, err := NewUploadTaskData(&source)
				if data != nil || !errors.Is(err, domain.ErrInvalidUploadTaskData) {
					t.Fatal("异常任务时间被接受", err)
				}
			})
		}
	}
}

func TestNewUploadTaskDataErrorSummaries(t *testing.T) {
	for _, fixture := range []struct{ code, publicCode, message string }{
		{"TARGET_UNAVAILABLE", "TARGET_UNAVAILABLE", "目标目录已不可用"},
		{"BODY_SIZE_MISMATCH", "BODY_SIZE_MISMATCH", "文件实际大小与声明不一致"},
		{"FILE_TOO_LARGE", "FILE_TOO_LARGE", "文件大小超过当前上传限制"},
		{"CHECKSUM_MISMATCH", "CHECKSUM_MISMATCH", "文件校验失败，请重新上传"},
		{"QUOTA_EXCEEDED", "QUOTA_EXCEEDED", "存储空间不足"},
		{"TEMP_SPACE_EXHAUSTED", "TEMP_SPACE_EXHAUSTED", "上传临时空间不足，请稍后重试"},
		{domain.UploadPartsMismatchCode, "UPLOAD_PARTS_MISMATCH", "分片回执不一致，请核对后补传"},
		{"STORAGE_UNAVAILABLE", "STORAGE_UNAVAILABLE", "存储服务暂时不可用"},
		{"DATABASE_UNAVAILABLE", "DATABASE_UNAVAILABLE", "数据服务暂时不可用"},
		{"INTERNAL_ERROR", "INTERNAL_ERROR", "上传任务处理异常"},
		{"STORAGE_RETRY", "INTERNAL_ERROR", "上传任务处理异常"},
		{"", "INTERNAL_ERROR", "上传任务处理异常"},
		{"private-upload-code /object?token=fixture", "INTERNAL_ERROR", "上传任务处理异常"},
		{"\xff", "INTERNAL_ERROR", "上传任务处理异常"},
	} {
		t.Run(fixture.code, func(t *testing.T) {
			// 1. 公共错误只由业务码生成，诊断信息保留在来源中。
			source := uploadTaskDataFixture()
			source.LastErrorCode = &fixture.code
			data, err := NewUploadTaskData(&source)
			if err != nil || data.LastError == nil || data.LastError.Code != fixture.publicCode || data.LastError.Message != fixture.message {
				t.Fatal("公开任务错误摘要不符", err)
			}
			encoded, err := json.Marshal(data)
			if err != nil || strings.Contains(string(encoded), "private-upload") || strings.Contains(string(encoded), "token=fixture") {
				t.Fatal("公开任务响应包含内部诊断或标识", err)
			}
		})
	}
	// 2. 未记录错误码时，诊断文本本身保持 last_error 为 null。
	source := uploadTaskDataFixture()
	data, err := NewUploadTaskData(&source)
	if err != nil || data.LastError != nil {
		t.Fatal("缺失错误码的公开摘要错误", err)
	}
}

func TestNewUploadTaskDataCopiesAndConcurrency(t *testing.T) {
	// 1. 生成带结果、终结时间及公开错误的两份数据。
	source := uploadTaskDataFixture()
	name, entryID, code, completedAt := "发布笔记.MD", uint64(31), "CHECKSUM_MISMATCH", source.UpdatedAt
	source.ResolvedName, source.ResultEntryID, source.LastErrorCode, source.CompletedAt = &name, &entryID, &code, &completedAt
	first, err := NewUploadTaskData(&source)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewUploadTaskData(&source)
	if err != nil {
		t.Fatal(err)
	}
	expectedJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if first.ResolvedName == source.ResolvedName || first.CompletedAt == source.CompletedAt ||
		first.ResolvedName == second.ResolvedName || first.ResultEntryID == second.ResultEntryID || first.CompletedAt == second.CompletedAt || first.LastError == second.LastError {
		t.Fatal("任务 DTO 共享可空对象")
	}
	*first.ResolvedName, *first.ResultEntryID, *first.CompletedAt, first.LastError.Message = "changed", "999", time.Time{}, "changed"
	if name != "发布笔记.MD" || entryID != 31 || completedAt != source.UpdatedAt || code != "CHECKSUM_MISMATCH" {
		t.Fatal("修改任务 DTO 影响了来源")
	}
	// 2. 并发转换共享只读来源，各自修改公开字段。
	var workers sync.WaitGroup
	failures := make(chan error, 32)
	for range 32 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 20 {
				data, err := NewUploadTaskData(&source)
				if err != nil {
					failures <- err
					return
				}
				encoded, err := json.Marshal(data)
				if err != nil || string(encoded) != string(expectedJSON) {
					failures <- fmt.Errorf("并发任务 DTO 转换错误")
					return
				}
				*data.ResolvedName, data.LastError.Message = "worker changed", "worker changed"
			}
		}()
	}
	workers.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
	// 3. 来源后续变化保持其他已生成响应的数据。
	name, entryID, completedAt, code = "source changed", 999, time.Time{}, "STORAGE_UNAVAILABLE"
	actualJSON, err := json.Marshal(second)
	if err != nil || string(actualJSON) != string(expectedJSON) {
		t.Fatal("来源或其他结果变化影响了已交付 DTO", err)
	}
}
