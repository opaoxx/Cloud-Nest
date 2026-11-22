// Package migrations contains the reviewed, versioned database schema.
package migrations

import (
	"crypto/sha256"
	_ "embed"
	"fmt"
)

// InitialSQL creates the eight initial business tables.
//
//go:embed 000001_initial.up.sql
var InitialSQL string

//go:embed 000002_upload_parts.up.sql
var uploadPartsSQL string

//go:embed 000003_sync_log.up.sql
var syncLogSQL string

// Migration 描述一份由程序内嵌的已评审迁移文件。
type Migration struct {
	Version uint64
	Name    string
	SQL     string
}

// Checksum 返回 SQL 原始字节的小写 SHA-256 十六进制摘要。
func (migration Migration) Checksum() string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(migration.SQL)))
}

// Ordered 返回按版本升序排列的迁移清单。每次调用均返回独立切片。
func Ordered() []Migration {
	return []Migration{
		{Version: 1, Name: "000001_initial.up.sql", SQL: InitialSQL},
		{Version: 2, Name: "000002_upload_parts.up.sql", SQL: uploadPartsSQL},
		{Version: 3, Name: "000003_sync_log.up.sql", SQL: syncLogSQL},
	}
}
