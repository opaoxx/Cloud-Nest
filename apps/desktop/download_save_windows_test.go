package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/opaoxx/cloud-nest/pkg/client"
	"golang.org/x/sys/windows"
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
	folder := t.TempDir()
	destination := filepath.Join(folder, "下载结果 01.bin")
	size := int64(len(content))
	sha := fmt.Sprintf("%x", sha256.Sum256(content))
	copyTo := func(path string) (string, error) {
		return copyForDownloadSave(t.Context(), io.NewSectionReader(source, 0, size), size, sha, path)
	}
	temporaryPath, err := copyTo(destination)
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
	if _, err := copyTo(destination); !errors.Is(err, errSaveTargetExists) {
		t.Fatalf("expected to reject the known conflict, got %v", err)
	}

	other := filepath.Join(folder, "下载结果 另存.bin")
	alternateTemp, err := copyTo(other)
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
	sha := fmt.Sprintf("%x", sha256.Sum256([]byte("abc")))
	if _, err := copyForDownloadSave(t.Context(), io.NewSectionReader(source, 0, 3), 3, sha, filepath.Join(folder, "new.bin")); err == nil ||
		err.Error() != "保存副本 SHA-256 校验失败，请重试" {
		t.Fatalf("corrupted copy was accepted: %v", err)
	}
	if entries, err := os.ReadDir(folder); err != nil || len(entries) != 0 {
		t.Fatalf("failed copy left a file in the target directory: %v", err)
	}
}

type downloadFaultReader func([]byte) (int, error)

func (read downloadFaultReader) Read(body []byte) (int, error) { return read(body) }

func TestDownloadSaveDiskFullKeepsVerifiedDataAndAllowsAnotherTarget(t *testing.T) {
	ctx := t.Context()
	directory := t.TempDir()
	db, err := openDownloadCheckpoints(ctx, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	body := []byte("verified download")
	auth := downloadAuthorization{owner: uploadOwner{Address: "http://127.0.0.1:18081", UserID: "7"}, entryID: "20",
		grant: client.DownloadGrant{ObjectID: "33", Revision: "1", SizeBytes: uint64(len(body)),
			SHA256: fmt.Sprintf("%x", sha256.Sum256(body))}}
	checkpoint, err := createDownloadCheckpoint(ctx, db, auth)
	if err != nil {
		t.Fatal(err)
	}
	temporary, err := openDownloadCheckpointFile(directory, checkpoint.fileID)
	if err != nil {
		t.Fatal(err)
	}
	defer temporary.Close()
	if _, err := temporary.Write(body); err != nil {
		t.Fatal(err)
	}
	writer := downloadCheckpointWriter{ctx: ctx, db: db, file: temporary, checkpoint: checkpoint, offset: int64(len(body))}
	if err := writer.flush(); err != nil {
		t.Fatal(err)
	}
	download := &stagedDownload{file: temporary, entryID: auth.entryID, checkpointID: checkpoint.fileID, result: downloadGrantView(auth.grant)}
	account := &Account{credentials: sessionFile{path: filepath.Join(directory, "session.bin")}, stagedDownload: download}
	targetFolder := t.TempDir()
	expectedTarget := filepath.Join(targetFolder, "result.bin")
	existing := filepath.Join(targetFolder, "existing.bin")
	original := []byte("keep this file")
	if err := os.WriteFile(existing, original, 0600); err != nil {
		t.Fatal(err)
	}

	// 1. 把真实副本写到一半，再返回 Windows 磁盘已满错误。
	var observedPartial bool
	prefix := body[:6]
	fault := downloadFaultReader(func([]byte) (int, error) {
		candidates, err := filepath.Glob(filepath.Join(targetFolder, ".cloudnest-save-*.part"))
		if err != nil || len(candidates) != 1 {
			return 0, fmt.Errorf("expected a single in-progress copy, found %v: %w", candidates, err)
		}
		info, err := os.Stat(candidates[0])
		observedPartial = err == nil && info.Size() == int64(len(prefix))
		return 0, &os.PathError{Op: "write", Path: candidates[0], Err: windows.ERROR_DISK_FULL}
	})
	if _, err := account.saveStagedDownload(ctx, download, io.MultiReader(bytes.NewReader(prefix), fault), expectedTarget); !errors.Is(err, errSaveDiskFull) || !observedPartial {
		t.Fatalf("space failure did not reach a partial copy: %v", err)
	}

	// 2. 清理目标临时副本，并保留完整的本机文件和失败原因。
	entries, err := os.ReadDir(targetFolder)
	if err != nil || len(entries) != 1 || entries[0].Name() != filepath.Base(existing) {
		t.Fatalf("failed save changed the destination: %v, %v", entries, err)
	}
	if actual, err := os.ReadFile(existing); err != nil || !bytes.Equal(actual, original) {
		t.Fatalf("existing target changed: %v", err)
	}
	persisted, err := readDownloadCheckpoint(ctx, db, auth)
	if err != nil || persisted.failure != errSaveDiskFull.code || persisted.confirmed != int64(len(body)) ||
		account.stagedDownload != download {
		t.Fatalf("verified checkpoint not preserved: %+v, %v", persisted, err)
	}

	// 3. 用同一份已校验的文件换位置另存，清除失败记录。
	alternate := filepath.Join(t.TempDir(), "success.bin")
	originalReader := io.NewSectionReader(temporary, 0, int64(len(body)))
	if saved, err := account.saveStagedDownload(ctx, download, originalReader, alternate); err != nil || saved != alternate {
		t.Fatalf("retry failed: %q, %v", saved, err)
	}
	if actual, err := os.ReadFile(alternate); err != nil || !bytes.Equal(actual, body) {
		t.Fatalf("saved file differs from verified download: %v", err)
	}
	var remaining int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM download_failures`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("failure record survived successful save: %d, %v", remaining, err)
	}
}
