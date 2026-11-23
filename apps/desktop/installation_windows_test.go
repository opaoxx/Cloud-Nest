package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestInstallationIDConcurrentFirstStart(t *testing.T) {
	localAppData := t.TempDir()
	t.Setenv("LOCALAPPDATA", localAppData)

	// 同一目录下的并发首次启动都必须读到最终保存的标识。
	type result struct {
		id  string
		err error
	}
	const starts = 12
	results := make(chan result, starts)
	for range starts {
		go func() {
			id, err := loadInstallationID()
			results <- result{id: id, err: err}
		}()
	}

	var winner string
	for range starts {
		value := <-results
		if value.err != nil {
			t.Errorf("loadInstallationID: %v", value.err)
			continue
		}
		if winner == "" {
			winner = value.id
		}
		if value.id != winner {
			t.Errorf("got %q, want shared ID %q", value.id, winner)
		}
	}
	id, err := uuid.Parse(winner)
	if err != nil || id.Version() != 4 || id.Variant() != uuid.RFC4122 || id.String() != winner {
		t.Fatalf("invalid installation ID: %q", winner)
	}
	path := filepath.Join(localAppData, "CloudNest", "installation-id")
	content, err := os.ReadFile(path)
	if err != nil || string(content) != winner {
		t.Fatalf("stored installation ID: %q, %v", content, err)
	}
	files, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(files) != 1 {
		t.Fatalf("expected only the installation-ID file, got %v, %v", files, err)
	}
}

func TestInvalidInstallationIDIsPreserved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "installation-id")
	invalidID := strings.ToUpper("c4ce88e7-fd87-4753-837c-145cb682728d")
	if err := os.WriteFile(path, []byte(invalidID), 0600); err != nil {
		t.Fatal(err)
	}
	if id, err := loadOrCreateInstallationID(path); id != "" || err == nil {
		t.Fatalf("invalid ID accepted: %q, %v", id, err)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != invalidID {
		t.Fatalf("existing installation ID changed: %q, %v", content, err)
	}
}
