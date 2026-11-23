package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/pkg/client"
)

func TestUploadIntentPersistsOnceAndIsScoped(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "草稿 % &.sqlite3")
	owner := uploadOwner{Address: "http://127.0.0.1:18081", UserID: "18446744073709551614"}
	draft := uploadIntentDraft{
		ParentID: "18446744073709551615",
		Source: selectedUploadSource{
			Path: filepath.Join(filepath.Dir(path), "上传资料 01.txt"),
			File: FileSelection{
				SourceFileDigest: SourceFileDigest{Name: "上传资料 01.txt", SHA256: strings.Repeat("a", 64)},
				CreationKey:      "c4ce88e7-fd87-4753-837c-145cb682728d",
			},
		},
	}
	db, err := openUploadIntents(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := insertUploadIntent(ctx, db, owner, draft); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	changed := draft
	changed.ParentID = "1"
	if err := insertUploadIntent(ctx, db, owner, changed); !errors.Is(err, errUploadKeyConflict) {
		db.Close()
		t.Fatalf("key accepted different destination: %v", err)
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
	if err != nil || len(items) != 1 || items[0] != (UploadIntentView{ID: "1", Name: draft.Source.File.Name, ParentID: draft.ParentID, Status: "PENDING_CREATE"}) {
		t.Fatalf("saved intent did not survive reopen: %+v, %v", items, err)
	}
	var sourcePath, sha256, status string
	err = db.QueryRowContext(ctx, `SELECT source_path, sha256, status FROM upload_intents WHERE creation_key = ?`,
		draft.Source.File.CreationKey).Scan(&sourcePath, &sha256, &status)
	if err != nil || sourcePath != draft.Source.Path || sha256 != draft.Source.File.SHA256 || status != "PENDING_CREATE" {
		t.Fatalf("source metadata or initial status was not preserved: %v", err)
	}
	for _, outsider := range []uploadOwner{
		{Address: owner.Address, UserID: "1"},
		{Address: "http://127.0.0.1:18082", UserID: owner.UserID},
	} {
		items, err := listUploadIntents(ctx, db, outsider)
		if err != nil || len(items) != 0 {
			t.Fatalf("another scope saw the intent: %+v, %v", items, err)
		}
	}
}

func TestUploadCreationKeepsTheFirstConfirmedReceipt(t *testing.T) {
	ctx := t.Context()
	db, err := openUploadIntents(ctx, filepath.Join(t.TempDir(), "drafts.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner := uploadOwner{Address: "http://127.0.0.1:18081", UserID: "1"}
	if err := insertUploadIntent(ctx, db, owner, uploadIntentDraft{
		ParentID: "20",
		Source: selectedUploadSource{
			Path: filepath.Join(t.TempDir(), "source.txt"),
			File: FileSelection{SourceFileDigest: SourceFileDigest{Name: "source.txt", SizeBytes: 5, SHA256: strings.Repeat("a", 64)},
				CreationKey: "test-create-key"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	receipt := client.SingleUploadCreation{TaskID: "33", ContentPath: "/api/v1/uploads/33/content", ExpiresAt: time.Now().UTC()}
	outsider := uploadOwner{Address: owner.Address, UserID: "2"}
	if err := recordUploadCreation(ctx, db, outsider, 1, receipt); err == nil {
		t.Fatal("another account recorded a receipt")
	}
	for range 2 {
		if err := recordUploadCreation(ctx, db, owner, 1, receipt); err != nil {
			t.Fatal(err)
		}
	}
	conflicting := receipt
	conflicting.TaskID = "34"
	if err := recordUploadCreation(ctx, db, owner, 1, conflicting); err == nil {
		t.Fatal("confirmed receipt was replaced")
	}
	items, err := listUploadIntents(ctx, db, owner)
	if err != nil || len(items) != 1 || items[0].Status != "CREATED" || items[0].TaskID != "33" {
		t.Fatalf("the first confirmed receipt was not preserved: %+v, %v", items, err)
	}
}

func TestUploadOutcomeIsAtomicAndDoesNotRegress(t *testing.T) {
	ctx := t.Context()
	db, err := openUploadIntents(ctx, filepath.Join(t.TempDir(), "outcomes.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	owner := uploadOwner{Address: "http://127.0.0.1:18081", UserID: "1"}
	if err := insertUploadIntent(ctx, db, owner, uploadIntentDraft{
		ParentID: "20",
		Source: selectedUploadSource{
			Path: filepath.Join(t.TempDir(), "source.txt"),
			File: FileSelection{SourceFileDigest: SourceFileDigest{Name: "source.txt", SHA256: strings.Repeat("a", 64)},
				CreationKey: "test-outcome-key"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := recordUploadCreation(ctx, db, owner, 1, client.SingleUploadCreation{
		TaskID: "33", ContentPath: "/api/v1/uploads/33/content", ExpiresAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	intent := scopedUploadIntent{db: db, owner: owner, id: 1}
	waiting := client.SingleUploadTaskSnapshot{Status: "VERIFYING"}
	published := client.SingleUploadTaskSnapshot{Status: "COMPLETED", ResultEntryID: "51"}
	if err := recordUploadOutcome(ctx, intent, "34", published); err == nil {
		t.Fatal("another remote task recorded an outcome")
	}
	if err := recordUploadOutcome(ctx, intent, "33", waiting); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER fail_outcome BEFORE INSERT ON single_upload_outcomes
		BEGIN SELECT RAISE(ABORT, 'test outcome insert failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := recordUploadOutcome(ctx, intent, "33", published); err == nil {
		t.Fatal("expected outcome insert to fail")
	}
	items, err := listUploadIntents(ctx, db, owner)
	if err != nil || len(items) != 1 || items[0].Status != "VERIFYING" || items[0].ResultEntryID != "" {
		t.Fatalf("failed insert left a partial outcome: %+v, %v", items, err)
	}
	if _, err := db.ExecContext(ctx, "DROP TRIGGER fail_outcome"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := recordUploadOutcome(ctx, intent, "33", published); err != nil {
			t.Fatal(err)
		}
	}
	if err := recordUploadOutcome(ctx, intent, "33", waiting); err == nil {
		t.Fatal("late verification snapshot overwrote a terminal state")
	}
	items, err = listUploadIntents(ctx, db, owner)
	if err != nil || len(items) != 1 || items[0].Status != "COMPLETED" || items[0].ResultEntryID != "51" {
		t.Fatalf("terminal state and entry were not preserved: %+v, %v", items, err)
	}
}
