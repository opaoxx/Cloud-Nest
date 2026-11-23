package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestDownloadSaveKeepsExistingTargetAndSupportsRetry(t *testing.T) {
	content := []byte{0, 255, 10, 253}
	sourcePath := filepath.Join(t.TempDir(), "staged.part")
	if err := os.WriteFile(sourcePath, content, 0600); err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	download := &stagedDownload{file: source, result: DownloadGrantResult{
		SizeBytes: uint64(len(content)), SHA256: fmt.Sprintf("%x", sha256.Sum256(content)),
	}}
	folder := t.TempDir()
	destination := filepath.Join(folder, "下载结果 01.bin")
	temporaryPath, err := copyForDownloadSave(t.Context(), download, destination)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(temporaryPath)

	// 目标在副本完成之后才出现，最终提交仍必须保留它。
	original := []byte("已有文件")
	if err := os.WriteFile(destination, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := publishDownload(temporaryPath, destination); !errors.Is(err, errSaveTargetExists) {
		t.Fatalf("expected a conflict during publication, got %v", err)
	}
	if actual, err := os.ReadFile(destination); err != nil || !bytes.Equal(actual, original) {
		t.Fatalf("existing target changed: %v", err)
	}
	if _, err := copyForDownloadSave(t.Context(), download, destination); !errors.Is(err, errSaveTargetExists) {
		t.Fatalf("expected to reject the known conflict, got %v", err)
	}

	other := filepath.Join(folder, "下载结果 另存.bin")
	alternateTemp, err := copyForDownloadSave(t.Context(), download, other)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(alternateTemp)
	if err := publishDownload(alternateTemp, other); err != nil {
		t.Fatal(err)
	}
	if actual, err := os.ReadFile(other); err != nil || !bytes.Equal(actual, content) {
		t.Fatalf("saved copy differs from the staging file: %v", err)
	}
}

func TestDownloadSaveRejectsInvalidCopy(t *testing.T) {
	source, err := os.CreateTemp(t.TempDir(), "staged")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if _, err := source.WriteString("bad"); err != nil {
		t.Fatal(err)
	}
	folder := t.TempDir()
	download := &stagedDownload{file: source, result: DownloadGrantResult{
		SizeBytes: 3, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("abc"))),
	}}
	if _, err := copyForDownloadSave(t.Context(), download, filepath.Join(folder, "new.bin")); err == nil ||
		err.Error() != "保存副本 SHA-256 校验失败，请重试" {
		t.Fatalf("corrupted copy was accepted: %v", err)
	}
	if entries, err := os.ReadDir(folder); err != nil || len(entries) != 0 {
		t.Fatalf("failed copy left a file in the target directory: %v", err)
	}
}
