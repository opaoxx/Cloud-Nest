package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
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
	if err != nil || len(items) != 1 || items[0] != (UploadIntentView{Name: draft.Source.File.Name, ParentID: draft.ParentID}) {
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
