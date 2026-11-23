package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/opaoxx/cloud-nest/pkg/client"
)

func TestOpenVerifiedUploadSourceChecksDigestAndRewinds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "验收 01.txt")
	expected := []byte("abc")
	if err := os.WriteFile(path, expected, 0600); err != nil {
		t.Fatal(err)
	}
	content := uploadContentSource{
		target: client.SingleUploadContentTarget{SizeBytes: int64(len(expected))},
		path:   path,
		sha256: fmt.Sprintf("%x", sha256.Sum256(expected)),
	}
	source, err := openVerifiedUploadSource(content)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(source)
	source.Close()
	if err != nil || string(body) != string(expected) {
		t.Fatalf("source was not rewound: %q, %v", body, err)
	}
	if err := os.WriteFile(path, []byte("xyz"), 0600); err != nil {
		t.Fatal(err)
	}
	source, err = openVerifiedUploadSource(content)
	if source != nil {
		source.Close()
		t.Fatal("same-size changed file reached the sender")
	}
	if err == nil || err.Error() != "源文件摘要已变化，请重新选择" {
		t.Fatalf("wrong verification result: %v", err)
	}
}
