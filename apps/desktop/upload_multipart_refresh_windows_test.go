package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opaoxx/cloud-nest/pkg/client"
)

func TestWaitForMultipartReadyPollsOriginalTaskAfterCreation(t *testing.T) {
	var taskReads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/api/v1/uploads/7" ||
			request.Header.Get("Authorization") != "Bearer access" {
			t.Errorf("unexpected readiness request: %s %s", request.Method, request.URL.Path)
		}
		status := "UPLOADING"
		if taskReads.Add(1) == 1 {
			status = "CREATING"
		}
		fmt.Fprintf(writer, `{"data":{"id":"7","parent_id":"3","upload_mode":"MULTIPART","size_bytes":20000000,"status":%q,"expires_at":%q}}`,
			status, time.Now().Add(time.Minute).UTC().Format(time.RFC3339))
	}))
	defer server.Close()
	intent := scopedUploadIntent{owner: uploadOwner{Address: server.URL}, accessToken: "access"}
	upload := multipartUploadSource{taskID: "7", parentID: "3", plan: client.MultipartPlan{SizeBytes: 20000000}}
	if err := waitForMultipartReady(t.Context(), intent, upload); err != nil {
		t.Fatal(err)
	}
	if calls := taskReads.Load(); calls != 2 {
		t.Fatalf("task reads = %d, want 2", calls)
	}
}

func TestUploadMultipartRefreshRetriesOnlyThisPartOnce(t *testing.T) {
	const expiredReply = "<Error><Code>AccessDenied</Code><Message>Request has expired</Message></Error>"
	const partBody = "this part"
	digest := sha256.Sum256([]byte(partBody))
	checksum := base64.StdEncoding.EncodeToString(digest[:])
	for _, fixture := range []struct {
		name      string
		signs     int32
		taskReads int32
		puts      int32
		success   bool
	}{
		{"storage expiry", 2, 1, 2, true},
		{"local expiry", 2, 1, 1, true},
		{"expired again", 2, 1, 2, false},
		{"task deadline passed", 1, 1, 1, false},
		{"permission denied", 1, 0, 1, false},
		{"session changed", 2, 1, 1, false},
		{"identical link", 2, 1, 1, false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			var signs, taskReads, puts atomic.Int32
			storage := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				body, err := io.ReadAll(request.Body)
				if err != nil || string(body) != partBody || request.ContentLength != int64(len(partBody)) ||
					request.Method != http.MethodPut || request.Header.Get("x-amz-checksum-sha256") != checksum ||
					request.Header.Get("Authorization") != "" || request.URL.Query().Get("partNumber") != "2" {
					t.Error("PUT did not preserve the source offset, bytes, checksum, and part")
				}
				puts.Add(1)
				if request.URL.Query().Get("grant") == "1" || fixture.name == "expired again" {
					writer.WriteHeader(http.StatusForbidden)
					if fixture.name == "permission denied" {
						_, _ = io.WriteString(writer, "<Error><Code>AccessDenied</Code><Message>Access denied</Message></Error>")
					} else {
						_, _ = io.WriteString(writer, expiredReply)
					}
					return
				}
				writer.Header().Set("ETag", "\"new-confirmed-tag\"")
			}))
			defer storage.Close()

			api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Header.Get("Authorization") != "Bearer sample" {
					t.Error("expected business authentication")
				}
				switch request.URL.Path {
				case "/api/v1/uploads/33":
					taskReads.Add(1)
					deadline := time.Now().Add(time.Hour)
					if fixture.name == "task deadline passed" {
						deadline = time.Now().Add(-time.Minute)
					}
					fmt.Fprintf(writer, `{"data":{"id":"33","parent_id":"20","upload_mode":"MULTIPART","size_bytes":16777225,
						"status":"UPLOADING","expires_at":%q}}`, deadline.Format(time.RFC3339Nano))
				case "/api/v1/uploads/33/part-urls":
					attempt := signs.Add(1)
					body, err := io.ReadAll(request.Body)
					if err != nil || string(body) != `{"parts":[{"part_number":2,"sha256":"`+hex.EncodeToString(digest[:])+`"}]}` {
						t.Error("request changed the task, part, checksum, or task deadline")
					}
					session := "original"
					if attempt == 2 && fixture.name == "session changed" {
						session = "different"
					}
					if fixture.name == "identical link" {
						attempt = 1
					}
					expires := time.Now().Add(time.Minute)
					if attempt == 1 && fixture.name == "local expiry" {
						expires = time.Now().Add(-time.Minute)
					}
					_ = json.NewEncoder(writer).Encode(map[string]any{"data": map[string]any{
						"expires_at": time.Now().Add(time.Hour),
						"parts": []any{map[string]any{"part_number": 2, "size_bytes": len(partBody),
							"url":              fmt.Sprintf("%s/object?partNumber=2&uploadId=%s&grant=%d", storage.URL, session, attempt),
							"required_headers": map[string]string{"x-amz-checksum-sha256": checksum},
							"url_expires_at":   expires}},
					}})
				default:
					t.Errorf("unexpected API request: %s", request.URL.Path)
					writer.WriteHeader(http.StatusNotFound)
				}
			}))
			defer api.Close()

			path := filepath.Join(t.TempDir(), "part.bin")
			if err := os.WriteFile(path, []byte("prefix"+partBody+"suffix"), 0600); err != nil {
				t.Fatal(err)
			}
			source, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			job := multipartPartJob{offsetBytes: int64(len("prefix")), declaration: client.UploadPartDeclaration{
				Number: 2, SizeBytes: int64(len(partBody)), Digest: digest,
			}}
			grant, err := client.IssueUploadPartURL(t.Context(), api.URL, "sample", "33", 2, int64(len(partBody)), digest)
			if err != nil {
				t.Fatal(err)
			}
			intent := scopedUploadIntent{owner: uploadOwner{Address: api.URL, UserID: "1"}, accessToken: "sample"}
			upload := multipartUploadSource{taskID: "33", parentID: "20", plan: client.MultipartPlan{SizeBytes: 16777225}}
			receipt, err := sendMultipartPart(t.Context(), intent, upload, source, job, grant)
			if (err == nil) != fixture.success || signs.Load() != fixture.signs || taskReads.Load() != fixture.taskReads || puts.Load() != fixture.puts {
				t.Fatalf("signs=%d, task checks=%d, puts=%d, err=%v", signs.Load(), taskReads.Load(), puts.Load(), err)
			}
			if fixture.success && (receipt.Number != 2 || receipt.ETag != "new-confirmed-tag") {
				t.Fatalf("wrong refreshed receipt: %+v", receipt)
			}
			if fixture.name == "expired again" && !errors.Is(err, client.ErrUploadPartURLExpired) {
				t.Fatalf("repeated expiry was lost: %v", err)
			}
		})
	}
}
