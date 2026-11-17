package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/internal/server/logging"
)

func TestServerUploadTempStartupFailures(t *testing.T) {
	for _, name := range []string{"empty config", "control character", "target is file", "parent is file", "canceled", "deadline"} {
		t.Run(name, func(t *testing.T) {
			// 1. 配置有效的前置参数，将测试目录及凭证限定在本用例。
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			closedAddress := listener.Addr().String()
			if err := listener.Close(); err != nil {
				t.Fatal(err)
			}
			secret := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
			for variable, value := range map[string]string{
				"CLOUDNEST_HTTP_ADDR":          closedAddress,
				"CLOUDNEST_DB_ADDRESS":         closedAddress,
				"CLOUDNEST_DB_NAME":            "startup_fixture",
				"CLOUDNEST_DB_USER":            "fixture-user",
				"CLOUDNEST_DB_PASSWORD":        "private-startup-db-password",
				"CLOUDNEST_STORAGE_PROVIDER":   "local_minio",
				"CLOUDNEST_S3_ENDPOINT":        "127.0.0.1:19000",
				"CLOUDNEST_S3_PUBLIC_ENDPOINT": "127.0.0.1:19000",
				"CLOUDNEST_S3_BUCKET":          "cloudnest",
				"CLOUDNEST_S3_ACCESS_KEY":      "fixture-access",
				"CLOUDNEST_S3_SECRET_KEY":      "private-startup-storage-key",
				"CLOUDNEST_ALLOWED_ORIGINS":    "http://127.0.0.1:5173",
				"CLOUDNEST_JWT_SECRET":         secret,
			} {
				t.Setenv(variable, value)
			}
			root := t.TempDir()
			directory := filepath.Join(root, "uncreated", "uploads")
			blocked := ""
			wantStage := "config"
			var ctx context.Context = context.Background()
			var want error
			switch name {
			case "empty config":
				directory = ""
			case "control character":
				directory = "private-startup-path\nuploads"
			case "target is file", "parent is file":
				blocked = filepath.Join(root, "ordinary-file")
				if err := os.WriteFile(blocked, []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
				directory = blocked
				if name == "parent is file" {
					directory = filepath.Join(blocked, "uploads")
				}
				wantStage = "upload_temp"
			case "canceled":
				canceled, cancel := context.WithCancel(context.Background())
				cancel()
				ctx, want, wantStage = canceled, context.Canceled, ""
			case "deadline":
				deadline, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer cancel()
				ctx, want, wantStage = deadline, context.DeadlineExceeded, "upload_temp"
			}
			t.Setenv("CLOUDNEST_TEMP_DIR", directory)
			// 2. 执行正式 run，核对失败阶段和后续启动的短路。
			var logs bytes.Buffer
			err = run(ctx, logging.New(&logs))
			if err == nil || (want != nil && !errors.Is(err, want)) {
				t.Fatal("启动失败原因错误", err)
			}
			if blocked != "" {
				var pathError *os.PathError
				if !errors.As(err, &pathError) {
					t.Fatal("文件系统原始原因丢失", err)
				}
				content, readError := os.ReadFile(blocked)
				if readError != nil || string(content) != "keep" {
					t.Fatal("启动检查改变已有文件", readError)
				}
			} else if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
				t.Fatal("无效或已取消启动创建了目录", err)
			}
			seenStage := ""
			for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
				var record struct {
					Level string `json:"level"`
					Stage string `json:"stage"`
				}
				if err := json.Unmarshal([]byte(line), &record); err != nil {
					t.Fatal("启动日志不是 JSON", err)
				}
				if record.Level == "ERROR" {
					seenStage = record.Stage
				}
			}
			if seenStage != wantStage {
				t.Fatal("启动日志错误阶段不符", seenStage, wantStage)
			}
			for _, absent := range []string{"JWT 配置读取成功", "MySQL 连接检查通过", "MySQL 连接失败", "准备启动 HTTP 服务", "上传临时目录检查通过", secret, "private-startup-db-password", "private-startup-storage-key", "private-startup-path"} {
				if strings.Contains(logs.String(), absent) {
					t.Fatal("失败后继续启动或日志包含私有输入")
				}
			}
		})
	}
}
