package main

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/pkg/client"
)

func TestDownloadCheckpointReconcilesFileAndVerifiesWholeObject(t *testing.T) {
	const content = "abcde"
	for _, fixture := range []struct {
		name, disk, requestRange string
		badDigest                bool
	}{
		{"uncommitted tail", "abextra", "bytes=2-", false},
		{"shorter than recorded", "a", "", false},
		{"damaged prefix", "zz", "bytes=2-", true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			ctx := t.Context()
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Header.Get("Range") != fixture.requestRange {
					t.Errorf("wrong Range: %q", request.Header.Get("Range"))
				}
				if fixture.requestRange != "" {
					writer.Header().Set("Content-Range", "bytes 2-4/5")
					writer.WriteHeader(http.StatusPartialContent)
					fmt.Fprint(writer, content[2:])
					return
				}
				fmt.Fprint(writer, content)
			}))
			defer server.Close()
			directory := t.TempDir()
			db, err := openDownloadCheckpoints(ctx, directory)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			authorization := downloadAuthorization{
				owner: uploadOwner{Address: "http://127.0.0.1:18081", UserID: "1"}, entryID: "20",
				grant: client.DownloadGrant{URL: server.URL, Revision: "2", ObjectID: "43",
					ExpiresAt: time.Now().Add(time.Minute), SizeBytes: uint64(len(content)),
					SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(content)))},
			}
			checkpoint, err := createDownloadCheckpoint(ctx, db, authorization)
			if err != nil {
				t.Fatal(err)
			}
			file, err := openDownloadCheckpointFile(directory, checkpoint.fileID)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if _, err := file.WriteString(fixture.disk); err != nil {
				t.Fatal(err)
			}
			transfer := downloadCheckpointWriter{ctx: ctx, db: db, file: file, checkpoint: checkpoint, offset: 2}
			if err := transfer.flush(); err != nil {
				t.Fatal(err)
			}
			otherAccount := authorization
			otherAccount.owner.UserID = "2"
			if _, err := readDownloadCheckpoint(ctx, db, otherAccount); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("another account can read the checkpoint: %v", err)
			}
			changedObject := authorization
			changedObject.grant.ObjectID = "44"
			if _, err := readDownloadCheckpoint(ctx, db, changedObject); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("another object can use the checkpoint: %v", err)
			}

			stageError := stageDownloadCheckpoint(&transfer, authorization.grant)
			saved, err := readDownloadCheckpoint(ctx, db, authorization)
			if err != nil {
				t.Fatal(err)
			}
			if fixture.badDigest {
				info, err := file.Stat()
				if stageError == nil || saved.confirmed != 0 || err != nil || info.Size() != 0 {
					t.Fatalf("corrupted prefix was kept or accepted: %+v, %v", saved, stageError)
				}
				return
			}
			body, err := io.ReadAll(io.NewSectionReader(file, 0, 20))
			if stageError != nil || err != nil || string(body) != content || saved.confirmed != int64(len(content)) {
				t.Fatalf("checkpoint did not produce the original object: %q, %+v, %v, %v", body, saved, stageError, err)
			}
		})
	}
}
