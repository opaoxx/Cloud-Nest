package main

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

type uploadOwner struct {
	Address string
	UserID  string
}

type uploadIntentDraft struct {
	Source   selectedUploadSource
	ParentID string
}

// UploadIntentView 是窗口展示的一条本地上传记录。
type UploadIntentView struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	SizeBytes     int64  `json:"size_bytes,string"`
	ParentID      string `json:"parent_id"`
	Status        string `json:"status"`
	TaskID        string `json:"task_id"`
	ResultEntryID string `json:"result_entry_id,omitempty"`
	ErrorCode     string `json:"error_code,omitempty"`
	ErrorMessage  string `json:"error_message,omitempty"`
}

var errUploadKeyConflict = errors.New("本次文件选择已保存到其他目标，请重新选择文件")

func openUploadIntents(ctx context.Context, path string) (*sql.DB, error) {
	// 1. 使用文件 URI 保留数据目录中的空格等字符，并为并发写入设置等待时间。
	if !filepath.IsAbs(path) {
		return nil, errors.New("无效的本机任务数据库路径")
	}
	uriPath := filepath.ToSlash(path)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	dsn := url.URL{Scheme: "file", Path: uriPath, RawQuery: "_busy_timeout=3000&_foreign_keys=1"}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, err
	}

	// 2. 创建桌面上传记录。账号和目录 ID 用文本保留完整的无符号范围。
	const schema = `CREATE TABLE IF NOT EXISTS upload_intents (
		id INTEGER PRIMARY KEY,
		server_address TEXT NOT NULL,
		user_id TEXT NOT NULL,
		parent_id TEXT NOT NULL,
		source_path TEXT NOT NULL,
		name TEXT NOT NULL,
		size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
		sha256 TEXT NOT NULL CHECK (length(sha256) = 64),
		creation_key TEXT NOT NULL UNIQUE,
		status TEXT NOT NULL DEFAULT 'PENDING_CREATE'
	) STRICT`
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, err
	}
	// 3. 独立保存确认回执，已有上传意图按原结构继续使用。
	const creationSchema = `CREATE TABLE IF NOT EXISTS single_upload_creations (
		intent_id INTEGER PRIMARY KEY REFERENCES upload_intents(id),
		task_id TEXT NOT NULL,
		content_path TEXT NOT NULL,
		expires_at TEXT NOT NULL
	) STRICT`
	if _, err := db.ExecContext(ctx, creationSchema); err != nil {
		db.Close()
		return nil, err
	}

	// 4. 补建终态结果表，旧任务和创建回执仍按原结构读取。
	const outcomeSchema = `CREATE TABLE IF NOT EXISTS single_upload_outcomes (
		intent_id INTEGER PRIMARY KEY REFERENCES single_upload_creations(intent_id),
		result_entry_id TEXT NOT NULL,
		error_code TEXT NOT NULL,
		error_message TEXT NOT NULL
	) STRICT`
	if _, err := db.ExecContext(ctx, outcomeSchema); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func insertUploadIntent(ctx context.Context, db *sql.DB, owner uploadOwner, draft uploadIntentDraft) error {
	// 1. 用唯一创建键插入，允许同一次选择在回执丢失后重新保存。
	file := draft.Source.File
	args := []any{owner.Address, owner.UserID, draft.ParentID, draft.Source.Path, file.Name, file.SizeBytes, file.SHA256, file.CreationKey}
	result, err := db.ExecContext(ctx, `INSERT INTO upload_intents
		(server_address, user_id, parent_id, source_path, name, size_bytes, sha256, creation_key)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(creation_key) DO NOTHING`, args...)
	if err != nil {
		return err
	}
	inserted, err := result.RowsAffected()
	if err != nil || inserted == 1 {
		return err
	}

	// 2. 同一个键只能绑定同一份声明，重复保存成功时沿用原记录。
	var exists bool
	err = db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM upload_intents WHERE server_address = ? AND user_id = ? AND parent_id = ?
		AND source_path = ? AND name = ? AND size_bytes = ? AND sha256 = ? AND creation_key = ?
	)`, args...).Scan(&exists)
	if err != nil {
		return err
	}
	if !exists {
		return errUploadKeyConflict
	}
	return nil
}

func listUploadIntents(ctx context.Context, db *sql.DB, owner uploadOwner) ([]UploadIntentView, error) {
	// 1. 只读取当前服务和当前账号的上传记录、创建回执和终态结果。
	rows, err := db.QueryContext(ctx, `SELECT CAST(i.id AS TEXT), i.name, i.size_bytes, i.parent_id, i.status, COALESCE(c.task_id, ''),
		COALESCE(o.result_entry_id, ''), COALESCE(o.error_code, ''), COALESCE(o.error_message, '')
		FROM upload_intents i
		LEFT JOIN single_upload_creations c ON c.intent_id = i.id
		LEFT JOIN single_upload_outcomes o ON o.intent_id = i.id
		WHERE i.server_address = ? AND i.user_id = ? ORDER BY i.id DESC`, owner.Address, owner.UserID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// 2. 返回窗口使用的字段，保留源路径、摘要和创建键在本机数据库。
	intents := make([]UploadIntentView, 0)
	for rows.Next() {
		var intent UploadIntentView
		if err := rows.Scan(&intent.ID, &intent.Name, &intent.SizeBytes, &intent.ParentID, &intent.Status, &intent.TaskID,
			&intent.ResultEntryID, &intent.ErrorCode, &intent.ErrorMessage); err != nil {
			return nil, err
		}
		intents = append(intents, intent)
	}
	return intents, rows.Err()
}
