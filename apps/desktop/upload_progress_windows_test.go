package main

import (
	"path/filepath"
	"testing"

	"github.com/opaoxx/cloud-nest/pkg/client"
)

func TestUploadProgressPreservesOnlyConfirmedBytes(t *testing.T) {
	const size = 12
	for _, fixture := range []struct {
		name, previous, next string
		initial, confirmed   int64
	}{
		{"created", "CREATED", "", 0, 0},
		{"legacy published", "COMPLETED", "", size, size},
		{"legacy failure", "FAILED", "", 0, 0},
		{"legacy receiving then failed", "VERIFYING", "FAILED", size, size},
		{"first body receipt", "CREATED", "VERIFYING", 0, size},
		{"published after lost receipt", "CREATED", "COMPLETED", 0, size},
		{"failure without receipt", "CREATED", "FAILED", 0, 0},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "transfers.sqlite3")
			owner := uploadOwner{Address: "http://127.0.0.1:18081", UserID: "1"}
			db, err := openUploadIntents(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			// 模拟旧版已保存的任务和创建回执，此时尚无任何进度行。
			if _, err := db.ExecContext(ctx, `INSERT INTO upload_intents
				(id, server_address, user_id, parent_id, source_path, name, size_bytes, sha256, creation_key, status)
				VALUES (1, ?, ?, '20', 'legacy.bin', 'legacy.bin', ?, printf('%064d',0), 'legacy-key', ?)`,
				owner.Address, owner.UserID, size, fixture.previous); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO single_upload_creations VALUES
				(1, '33', '/api/v1/uploads/33/content', '2026-11-24T03:00:00Z')`); err != nil {
				t.Fatal(err)
			}
			items, err := listUploadIntents(ctx, db, owner)
			if err != nil || len(items) != 1 || items[0].ConfirmedBytes != fixture.initial {
				t.Fatalf("initial progress = %+v, %v", items, err)
			}

			if fixture.next != "" {
				intent := scopedUploadIntent{db: db, owner: owner, id: 1}
				snapshot := client.SingleUploadTaskSnapshot{Status: fixture.next}
				if fixture.next == "COMPLETED" {
					snapshot.ResultEntryID = "51"
				}
				if err := recordUploadOutcome(ctx, intent, "33", snapshot); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = openUploadIntents(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			items, err = listUploadIntents(ctx, db, owner)
			if err != nil || len(items) != 1 || items[0].ConfirmedBytes != fixture.confirmed {
				t.Fatalf("progress after reopen = %+v, %v", items, err)
			}
			if fixture.next != "" {
				var stored int64
				err = db.QueryRowContext(ctx, `SELECT COALESCE(SUM(confirmed_bytes), 0) FROM single_upload_confirmations`).Scan(&stored)
				if err != nil || stored != fixture.confirmed {
					t.Fatalf("persisted progress = %d, %v", stored, err)
				}
			}
		})
	}
}
