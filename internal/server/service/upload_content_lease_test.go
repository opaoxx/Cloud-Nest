package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestValidateUploadContentLeaseResults(t *testing.T) {
	for _, mode := range []string{"initial", "released", "expired", "exact expiry", "one nanosecond after", "maximum next version", "maximum IDs", "zero bytes", "single limit", "owner length limit", "timezone", "minimum lease year", "maximum lease year"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 准备已通过接收阶段检查的任务，按场景设置旧租约。
			task, now := uploadContentStateFixture()
			owner, until := "fixture-worker", now.Add(-time.Minute)
			task.LeaseOwner, task.LeaseUntil, task.LeaseVersion = &owner, &until, 3
			switch mode {
			case "initial":
				task.LeaseOwner, task.LeaseUntil, task.LeaseVersion = nil, nil, 0
			case "released":
				task.LeaseOwner, task.LeaseUntil = nil, nil
			case "exact expiry":
				until = now
			case "one nanosecond after":
				until, now = now, now.Add(time.Nanosecond)
			case "maximum next version":
				task.LeaseVersion = ^uint64(0) - 1
			case "maximum IDs":
				task.ID, task.UserID, task.DeviceID, task.StagedObjectID, task.TargetParentID = ^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0), ^uint64(0)
			case "zero bytes":
				task.FileSize, task.ReservedBytes = 0, 0
			case "single limit":
				task.FileSize, task.ReservedBytes = domain.MaxSingleUploadSizeBytes, domain.MaxSingleUploadSizeBytes
			case "owner length limit":
				owner = strings.Repeat("a", 64)
			case "timezone":
				zone := time.FixedZone("fixture UTC+8", 8*3600)
				until, now = now.In(zone), now.In(zone)
			case "minimum lease year":
				until = time.Date(1000, 1, 1, 0, 0, 0, 0, time.UTC)
			case "maximum lease year":
				until = time.Date(9999, 12, 31, 23, 59, 59, 999998000, time.UTC)
				now, task.ExpiresAt = until, until.Add(time.Microsecond)
			}
			// 2. 校验通过，所有输入字段继续保持原值。
			before, err := json.Marshal(task)
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateUploadContentLease(&task, task.UserID, task.ID, now); err != nil {
				t.Fatal("可领取接收租约被拒绝", err)
			}
			after, err := json.Marshal(task)
			if err != nil || string(after) != string(before) {
				t.Fatal("租约校验修改了输入", err)
			}
		})
	}
}

func TestValidateUploadContentLeaseBusy(t *testing.T) {
	for _, mode := range []string{"active", "one nanosecond before", "timezone", "maximum version busy first"} {
		t.Run(mode, func(t *testing.T) {
			task, now := uploadContentStateFixture()
			owner, until := "fixture-worker", now.Add(time.Minute)
			task.LeaseOwner, task.LeaseUntil, task.LeaseVersion = &owner, &until, 42
			switch mode {
			case "one nanosecond before":
				now = until.Add(-time.Nanosecond)
			case "timezone":
				until = until.In(time.FixedZone("fixture UTC+8", 8*3600))
			case "maximum version busy first":
				task.LeaseVersion = ^uint64(0)
			}
			err := ValidateUploadContentLease(&task, task.UserID, task.ID, now)
			if !errors.Is(err, domain.ErrUploadTaskBusy) || err.Error() != "上传任务正在处理中" || !errors.Is(fmt.Errorf("wrapped: %w", err), domain.ErrUploadTaskBusy) {
				t.Fatal("有效租约未返回固定忙碌错误", err)
			}
		})
	}
}

func TestValidateUploadContentLeaseInvalidData(t *testing.T) {
	for _, mode := range []string{"owner only", "until only", "empty owner", "long owner", "non ASCII owner", "invalid UTF8 owner", "zero claimed version", "zero until", "year below", "year above", "UTC year overflow", "submicrosecond until", "unleased maximum", "expired maximum", "exact expiry maximum"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 分别注入租约事实异常及无法递增的版本。
			task, now := uploadContentStateFixture()
			owner, until := "fixture-worker", now.Add(-time.Minute)
			task.LeaseOwner, task.LeaseUntil, task.LeaseVersion = &owner, &until, 42
			wantError := domain.ErrInvalidUploadTaskData
			switch mode {
			case "owner only":
				task.LeaseUntil = nil
			case "until only":
				task.LeaseOwner = nil
			case "empty owner":
				owner = ""
			case "long owner":
				owner = strings.Repeat("a", 65)
			case "non ASCII owner":
				owner = "执行者"
			case "invalid UTF8 owner":
				owner = "\xff"
			case "zero claimed version":
				task.LeaseVersion = 0
			case "zero until":
				until = time.Time{}
			case "year below":
				until = time.Date(999, 12, 31, 23, 59, 59, 0, time.UTC)
			case "year above":
				until = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
			case "UTC year overflow":
				until = time.Date(1000, 1, 1, 0, 0, 0, 0, time.FixedZone("fixture UTC+8", 8*3600))
			case "submicrosecond until":
				until = now.Add(time.Nanosecond)
			case "unleased maximum":
				task.LeaseOwner, task.LeaseUntil, task.LeaseVersion = nil, nil, ^uint64(0)
				wantError = domain.ErrUploadLeaseVersionExhausted
			case "expired maximum":
				task.LeaseVersion, wantError = ^uint64(0), domain.ErrUploadLeaseVersionExhausted
			case "exact expiry maximum":
				until, task.LeaseVersion, wantError = now, ^uint64(0), domain.ErrUploadLeaseVersionExhausted
			}
			// 2. 异常事实和版本上限可通过包装错误继续识别。
			err := ValidateUploadContentLease(&task, task.UserID, task.ID, now)
			if !errors.Is(err, wantError) || !errors.Is(fmt.Errorf("wrapped: %w", err), wantError) {
				t.Fatal("异常租约或版本上限错误", err)
			}
			if wantError == domain.ErrUploadLeaseVersionExhausted && err.Error() != "上传租约版本已达上限" {
				t.Fatal("版本上限提示不一致")
			}
		})
	}
}

func TestValidateUploadContentLeaseInputsAndOrder(t *testing.T) {
	// 1. 基础调用错误、记录范围和接收阶段沿用已有校验顺序。
	task, now := uploadContentStateFixture()
	for _, fixture := range []struct {
		userID, taskID uint64
		now            time.Time
	}{{0, 19, now}, {7, 0, now}, {7, 19, time.Time{}}} {
		if err := ValidateUploadContentLease(&task, fixture.userID, fixture.taskID, fixture.now); err == nil || err.Error() != "校验上传接收状态需要非零用户和任务 ID 及判断时间" {
			t.Fatal("基础调用条件未复用", err)
		}
	}
	if err := ValidateUploadContentLease(nil, 7, 19, now); !errors.Is(err, domain.ErrInvalidUploadTaskData) {
		t.Fatal("空任务未拒绝", err)
	}
	if err := ValidateUploadContentLease(&task, 8, 19, now); !errors.Is(err, domain.ErrInvalidUploadTaskData) {
		t.Fatal("任务范围未先校验", err)
	}
	// 2. 任务期限和阶段先于租约，大小与预占先于租约事实。
	owner, until := "fixture-worker", now.Add(time.Minute)
	task.LeaseOwner, task.LeaseUntil, task.LeaseVersion = &owner, &until, ^uint64(0)
	task.ExpiresAt = now
	if err := ValidateUploadContentLease(&task, 7, 19, now); !errors.Is(err, domain.ErrUploadTaskNotReady) {
		t.Fatal("任务到期顺序错误", err)
	}
	task.ExpiresAt = now.Add(time.Hour)
	task.Status = domain.UploadStatusVerifying
	if err := ValidateUploadContentLease(&task, 7, 19, now); !errors.Is(err, domain.ErrUploadTaskNotReady) {
		t.Fatal("任务阶段顺序错误", err)
	}
	task.Status, task.FileSize = domain.UploadStatusUploading, domain.MaxSingleUploadSizeBytes+1
	if err := ValidateUploadContentLease(&task, 7, 19, now); !errors.Is(err, domain.ErrUploadFileTooLarge) {
		t.Fatal("声明大小顺序错误", err)
	}
	task.FileSize, task.ReservedBytes, owner = 2048, 2047, ""
	if err := ValidateUploadContentLease(&task, 7, 19, now); !errors.Is(err, domain.ErrInvalidUploadTaskData) {
		t.Fatal("异常接收记录未拒绝", err)
	}
}

func TestValidateUploadContentLeaseIndependent(t *testing.T) {
	for _, mode := range []string{"initial", "expired", "busy", "exhausted"} {
		t.Run(mode, func(t *testing.T) {
			// 1. 深复制租约、摘要和诊断，随后并发执行纯校验。
			task, now := uploadContentStateFixture()
			owner, until, diagnostic := "fixture-worker", now.Add(-time.Minute), "private-diagnostic"
			task.LastErrorMessage = &diagnostic
			var wantError error
			if mode != "initial" {
				task.LeaseOwner, task.LeaseUntil, task.LeaseVersion = &owner, &until, 42
			}
			switch mode {
			case "busy":
				until, wantError = now.Add(time.Minute), domain.ErrUploadTaskBusy
			case "exhausted":
				task.LeaseVersion, wantError = ^uint64(0), domain.ErrUploadLeaseVersionExhausted
			}
			body, err := json.Marshal(task)
			if err != nil {
				t.Fatal(err)
			}
			var original domain.UploadTask
			if err := json.Unmarshal(body, &original); err != nil {
				t.Fatal(err)
			}
			var workers sync.WaitGroup
			for worker := 0; worker < 32; worker++ {
				workers.Add(1)
				go func() {
					defer workers.Done()
					for attempt := 0; attempt < 20; attempt++ {
						err := ValidateUploadContentLease(&task, task.UserID, task.ID, now)
						if !errors.Is(err, wantError) {
							t.Error("并发租约校验结果错误", err)
						}
					}
				}()
			}
			workers.Wait()
			// 2. 对照完整副本，确认版本、可空字段和摘要均保留原值。
			if !reflect.DeepEqual(task, original) {
				t.Fatal("租约校验修改了来源任务")
			}
		})
	}
}
