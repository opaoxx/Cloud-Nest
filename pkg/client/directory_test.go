package client

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadDirectoryPageUsesSelectedIDAndServerPath(t *testing.T) {
	const targetID = "18446744073709551614"
	for _, fixture := range []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{
			name:   "nested folder",
			status: http.StatusOK,
			body: `{"data":{"items":[],"breadcrumbs":[{"id":"1","name":""},{"id":"30","name":"课程资料"},` +
				`{"id":"` + targetID + `","name":"第一章"}],"next_cursor":null}}`,
		},
		{
			name:    "unavailable folder",
			status:  http.StatusNotFound,
			body:    `{"error":{"code":"ENTRY_NOT_FOUND"}}`,
			wantErr: "目录不存在或已不可访问",
		},
		{
			name:    "wrong path endpoint",
			status:  http.StatusOK,
			body:    `{"data":{"items":[],"breadcrumbs":[{"id":"1","name":""}],"next_cursor":null}}`,
			wantErr: "服务返回的目录页与请求不一致",
		},
		{
			name:   "repeated ancestor",
			status: http.StatusOK,
			body: `{"data":{"items":[],"breadcrumbs":[{"id":"1","name":""},{"id":"1","name":"重复的目录"},` +
				`{"id":"` + targetID + `","name":"第一章"}],"next_cursor":null}}`,
			wantErr: "服务返回的目录路径有误",
		},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.Method != http.MethodGet || request.URL.RequestURI() != "/api/v1/entries?parent_id="+targetID ||
					request.Header.Get("Authorization") != "Bearer sample" {
					t.Errorf("unexpected request: %s %s", request.Method, request.URL)
				}
				writer.WriteHeader(fixture.status)
				fmt.Fprint(writer, fixture.body)
			}))
			defer server.Close()
			page, err := ReadDirectoryPage(t.Context(), server.URL, "sample", targetID)
			if fixture.wantErr != "" {
				if err == nil || err.Error() != fixture.wantErr || page.Breadcrumbs != nil {
					t.Fatalf("unexpected error result: %+v, %v", page, err)
				}
				return
			}
			if err != nil || len(page.Breadcrumbs) != 3 || page.Breadcrumbs[1].Name != "课程资料" ||
				page.Breadcrumbs[2] != (DirectoryBreadcrumb{ID: targetID, Name: "第一章"}) || page.Items == nil || len(page.Items) != 0 {
				t.Fatalf("server path was not preserved: %+v, %v", page, err)
			}
		})
	}
}

func TestReadNextDirectoryPagePreservesCursor(t *testing.T) {
	const cursor = "opaque+/=%20"
	for _, fixture := range []struct {
		name       string
		nextCursor string
		wantError  bool
	}{
		{name: "last page", nextCursor: "null"},
		{name: "repeated cursor", nextCursor: fmt.Sprintf("%q", cursor), wantError: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				query := request.URL.Query()
				if request.Method != http.MethodGet || request.URL.Path != "/api/v1/entries" ||
					query.Get("parent_id") != "12" || query.Get("cursor") != cursor || len(query["cursor"]) != 1 {
					t.Error("continuation request changed the directory ID or cursor")
				}
				fmt.Fprintf(writer, `{"data":{"items":[{"id":"42","parent_id":"12","name":"续页文件","entry_type":"FILE","revision":"18446744073709551615","size_bytes":0}],`+
					`"breadcrumbs":[{"id":"1","name":""},{"id":"12","name":"资料"}],"next_cursor":%s}}`, fixture.nextCursor)
			}))
			defer server.Close()
			page, err := ReadNextDirectoryPage(t.Context(), server.URL, "sample", "12", cursor)
			if fixture.wantError {
				if err == nil || page.Items != nil {
					t.Fatalf("repeated cursor was accepted: %+v, %v", page, err)
				}
				return
			}
			if err != nil || page.NextCursor != nil || len(page.Items) != 1 || page.Items[0].ID != "42" ||
				page.Items[0].Revision != "18446744073709551615" {
				t.Fatalf("expected the last page: %+v, %v", page, err)
			}
		})
	}
}
