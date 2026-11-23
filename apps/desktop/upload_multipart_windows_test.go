package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/pkg/client"
	"golang.org/x/sys/windows"
)

func TestUploadMultipartPlanSurvivesReopenAndIsScoped(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "transfers.sqlite3")
	owner := uploadOwner{Address: "http://127.0.0.1:18081", UserID: "1"}
	db, err := openUploadIntents(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const size = client.MaxSingleUploadBytes + 1
	if _, err := db.ExecContext(ctx, `INSERT INTO upload_intents
		(id, server_address, user_id, parent_id, source_path, name, size_bytes, sha256, creation_key)
		VALUES (1, ?, ?, '20', 'multipart.bin', 'multipart.bin', ?, printf('%064d',0), 'test-multipart')`,
		owner.Address, owner.UserID, size); err != nil {
		t.Fatal(err)
	}
	receipt := client.UploadCreation{TaskID: "33", ExpiresAt: time.Now().UTC(),
		Multipart: &client.MultipartPlan{SizeBytes: size, PartSizeBytes: client.MaxSingleUploadBytes, PartCount: 2}}
	other := uploadOwner{Address: owner.Address, UserID: "2"}
	if err := recordMultipartUploadCreation(ctx, scopedUploadIntent{db: db, owner: other, id: 1}, receipt); err == nil {
		t.Fatal("different account recorded a multipart plan")
	}
	intent := scopedUploadIntent{db: db, owner: owner, id: 1}
	for range 2 {
		if err := recordMultipartUploadCreation(ctx, intent, receipt); err != nil {
			t.Fatal(err)
		}
	}
	conflict := receipt
	conflict.Multipart = &client.MultipartPlan{SizeBytes: size, PartSizeBytes: 2 * client.MaxSingleUploadBytes, PartCount: 1}
	if err := recordMultipartUploadCreation(ctx, intent, conflict); err == nil {
		t.Fatal("existing multipart plan was replaced")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = openUploadIntents(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	items, err := listUploadIntents(ctx, db, owner)
	if err != nil || len(items) != 1 {
		t.Fatalf("saved plan missing: %+v, %v", items, err)
	}
	item := items[0]
	wantPlan := MultipartPlanView{PartCount: 2, PartSizeBytes: client.MaxSingleUploadBytes,
		LastPartOffsetBytes: client.MaxSingleUploadBytes, LastPartSizeBytes: 1}
	if item.ConfirmedBytes != 0 || item.TaskID != receipt.TaskID || item.Status != "MULTIPART_CREATED" ||
		item.MultipartPlan == nil || *item.MultipartPlan != wantPlan {
		t.Fatalf("reopened plan does not preserve the receipt: %+v", item)
	}
	if items, err := listUploadIntents(ctx, db, other); err != nil || len(items) != 0 {
		t.Fatalf("different account saw the saved plan: %+v, %v", items, err)
	}

	intent.db = db
	part := client.UploadPartReceipt{Number: 1, SizeBytes: client.MaxSingleUploadBytes, SHA256: strings.Repeat("a", 64), ETag: "part-etag"}
	incorrect := part
	incorrect.SizeBytes--
	if err := recordMultipartPart(ctx, intent, receipt.TaskID, incorrect); err == nil {
		t.Fatal("receipt with the wrong size was accepted")
	}
	for range 2 {
		if err := recordMultipartPart(ctx, intent, receipt.TaskID, part); err != nil {
			t.Fatal(err)
		}
	}
	incorrect = part
	incorrect.ETag = "different"
	if err := recordMultipartPart(ctx, intent, receipt.TaskID, incorrect); err == nil {
		t.Fatal("first receipt was replaced")
	}
	items, err = listUploadIntents(ctx, db, owner)
	if err != nil || len(items) != 1 || items[0].ConfirmedBytes != part.SizeBytes ||
		items[0].MultipartPlan == nil || items[0].MultipartPlan.ConfirmedParts != 1 {
		t.Fatalf("progress did not account for the one saved receipt: %+v, %v", items, err)
	}
}

func TestUploadMultipartQueueLockPreventsSecondDispatcher(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transfers.sqlite3.parts.lock")
	first, err := openExclusiveFileLock(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := openExclusiveFileLock(path)
	if second != nil {
		second.Close()
	}
	if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		t.Fatalf("second dispatcher should fail, got %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err = openExclusiveFileLock(path)
	if err != nil {
		t.Fatalf("dispatcher could not reacquire the lock: %v", err)
	}
	second.Close()
}
