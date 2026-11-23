package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestReadSourceFileDigest(t *testing.T) {
	for _, sample := range []struct {
		name    string
		content []byte
	}{
		{name: "空文件.txt"},
		{name: "课程资料 01.bin", content: bytes.Repeat([]byte{0, 1, 255, 10, 13}, 30000)},
	} {
		t.Run(sample.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), sample.name)
			if err := os.WriteFile(path, sample.content, 0600); err != nil {
				t.Fatal(err)
			}
			got, err := readSourceFileDigest(t.Context(), path)
			if err != nil {
				t.Fatal(err)
			}
			want := SourceFileDigest{
				Name:      sample.name,
				SizeBytes: int64(len(sample.content)),
				SHA256:    fmt.Sprintf("%x", sha256.Sum256(sample.content)),
			}
			if *got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		})
	}
}

func TestReadSourceFileDigestRejectsInvalidPaths(t *testing.T) {
	directory := t.TempDir()
	for _, path := range []string{"relative.txt", directory, filepath.Join(directory, "missing.txt")} {
		if digest, err := readSourceFileDigest(t.Context(), path); err == nil || digest != nil {
			t.Fatalf("accepted invalid source %q: %+v, %v", path, digest, err)
		}
	}
}
