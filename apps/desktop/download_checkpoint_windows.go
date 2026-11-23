package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/pkg/client"
	"golang.org/x/sys/windows"
)

const downloadCheckpointInterval = 1 << 20

type downloadCheckpoint struct {
	fileID    string
	sizeBytes int64
	sha256    string
	confirmed int64
}

type downloadAuthorization struct {
	owner   uploadOwner
	entryID string
	grant   client.DownloadGrant
}

func openDownloadCheckpoints(ctx context.Context, directory string) (*sql.DB, error) {
	db, err := openTransferDatabase(filepath.Join(directory, "transfers.sqlite3"))
	if err != nil {
		return nil, errors.New("无法打开本机下载记录")
	}
	const schema = `CREATE TABLE IF NOT EXISTS download_checkpoints (
		file_id TEXT PRIMARY KEY,
		server_address TEXT NOT NULL,
		user_id TEXT NOT NULL,
		entry_id TEXT NOT NULL,
		revision TEXT NOT NULL,
		object_id TEXT NOT NULL,
		size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
		sha256 TEXT NOT NULL CHECK (length(sha256) = 64),
		confirmed_bytes INTEGER NOT NULL DEFAULT 0 CHECK (confirmed_bytes BETWEEN 0 AND size_bytes),
		UNIQUE (server_address, user_id, entry_id, revision, object_id)
	) STRICT`
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, errors.New("无法打开本机下载记录")
	}
	return db, nil
}

func readDownloadCheckpoint(ctx context.Context, db *sql.DB, authorization downloadAuthorization) (downloadCheckpoint, error) {
	var checkpoint downloadCheckpoint
	grant := authorization.grant
	err := db.QueryRowContext(ctx, `SELECT file_id, size_bytes, sha256, confirmed_bytes FROM download_checkpoints
		WHERE server_address = ? AND user_id = ? AND entry_id = ? AND revision = ? AND object_id = ?`,
		authorization.owner.Address, authorization.owner.UserID, authorization.entryID, grant.Revision, grant.ObjectID).
		Scan(&checkpoint.fileID, &checkpoint.sizeBytes, &checkpoint.sha256, &checkpoint.confirmed)
	if errors.Is(err, sql.ErrNoRows) {
		return checkpoint, err
	}
	if err != nil {
		return checkpoint, errors.New("无法读取本机下载断点")
	}
	if checkpoint.sizeBytes != int64(grant.SizeBytes) || checkpoint.sha256 != grant.SHA256 {
		return checkpoint, errors.New("下载对象的大小或摘要已变化，请刷新目录后重新选择")
	}
	return checkpoint, nil
}

func createDownloadCheckpoint(ctx context.Context, db *sql.DB, authorization downloadAuthorization) (downloadCheckpoint, error) {
	fileID, err := uuid.NewRandom()
	if err != nil {
		return downloadCheckpoint{}, errors.New("无法生成下载临时文件名")
	}
	grant := authorization.grant
	checkpoint := downloadCheckpoint{fileID: fileID.String(), sizeBytes: int64(grant.SizeBytes), sha256: grant.SHA256}
	_, err = db.ExecContext(ctx, `INSERT INTO download_checkpoints
		(file_id, server_address, user_id, entry_id, revision, object_id, size_bytes, sha256)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		checkpoint.fileID, authorization.owner.Address, authorization.owner.UserID, authorization.entryID,
		grant.Revision, grant.ObjectID, checkpoint.sizeBytes, checkpoint.sha256)
	if err != nil {
		return downloadCheckpoint{}, errors.New("无法保存本机下载记录")
	}
	return checkpoint, nil
}

func lockDownloadCheckpoints(directory string) (*os.File, error) {
	if !filepath.IsAbs(directory) {
		return nil, errors.New("无法确定本机下载临时目录")
	}
	lock, err := openExclusiveFileLock(filepath.Join(directory, "transfers.sqlite3.downloads.lock"))
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return nil, errors.New("另一个窗口正在处理下载，请稍后重试")
	}
	if err != nil {
		return nil, errors.New("无法锁定本机下载记录")
	}
	return lock, nil
}

func openDownloadCheckpointFile(directory, fileID string) (*os.File, error) {
	id, err := uuid.Parse(fileID)
	if err != nil || id.String() != fileID {
		return nil, errors.New("无效的下载临时文件名")
	}
	path := filepath.Join(directory, ".download-"+fileID+".part")
	nativePath, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, errors.New("无效的下载临时文件路径")
	}
	handle, err := windows.CreateFile(nativePath, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ, nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, errors.New("无法打开下载临时文件，请检查本机存储或稍后重试")
	}
	return os.NewFile(uintptr(handle), path), nil
}

// downloadCheckpointWriter 在文件内容同步到磁盘后才公布新的断点。
type downloadCheckpointWriter struct {
	ctx        context.Context
	db         *sql.DB
	file       *os.File
	checkpoint downloadCheckpoint
	offset     int64
}

func (writer *downloadCheckpointWriter) restore() error {
	// 1. 上次退出时，落盘偏移之后的字节尚未确认，截掉后从有效位置继续。
	info, err := writer.file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("无法检查下载临时文件")
	}
	if info.Size() < writer.checkpoint.confirmed {
		return writer.restart()
	}
	if info.Size() > writer.checkpoint.confirmed {
		if err := writer.file.Truncate(writer.checkpoint.confirmed); err != nil {
			return errors.New("无法整理下载临时文件")
		}
	}
	writer.offset = writer.checkpoint.confirmed
	return writer.seek()
}

func (writer *downloadCheckpointWriter) restart() error {
	// 1. 先降低已确认偏移。即使此时退出，下次也会丢弃未经确认的旧字节。
	if err := writer.storeOffset(0); err != nil {
		return err
	}
	writer.offset = 0
	if err := writer.file.Truncate(0); err != nil {
		return errors.New("无法重新开始写入下载临时文件")
	}
	return writer.seek()
}

func (writer *downloadCheckpointWriter) seek() error {
	if _, err := writer.file.Seek(writer.offset, io.SeekStart); err != nil {
		return errors.New("无法定位下载临时文件")
	}
	return nil
}

func (writer *downloadCheckpointWriter) Write(body []byte) (int, error) {
	if err := writer.ctx.Err(); err != nil {
		return 0, err
	}
	written, err := writer.file.Write(body)
	writer.offset += int64(written)
	if err == nil && writer.offset < writer.checkpoint.sizeBytes &&
		writer.offset-writer.checkpoint.confirmed >= downloadCheckpointInterval {
		err = writer.flush()
	}
	return written, err
}

func (writer *downloadCheckpointWriter) flush() error {
	if writer.offset == writer.checkpoint.confirmed {
		return nil
	}
	if err := writer.file.Sync(); err != nil {
		return errors.New("无法同步下载临时文件，请检查本机存储")
	}
	return writer.storeOffset(writer.offset)
}

func (writer *downloadCheckpointWriter) storeOffset(offset int64) error {
	if offset == writer.checkpoint.confirmed {
		return nil
	}
	result, err := writer.db.ExecContext(writer.ctx, `UPDATE download_checkpoints SET confirmed_bytes = ?
		WHERE file_id = ? AND confirmed_bytes = ?`, offset, writer.checkpoint.fileID, writer.checkpoint.confirmed)
	if err != nil {
		return errors.New("无法保存本机下载断点")
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		return errors.New("本机下载断点已变化，请重试")
	}
	writer.checkpoint.confirmed = offset
	return nil
}

func stageDownloadCheckpoint(writer *downloadCheckpointWriter, grant client.DownloadGrant) error {
	// 1. 核对断点与临时文件。文件缺失或不够长时自动从零开始。
	if err := writer.restore(); err != nil {
		return err
	}
	size := writer.checkpoint.sizeBytes
	if writer.offset != size || size == 0 {
		// 2. 存储返回正确范围才追加，返回完整正文时先清除旧断点。
		err := client.DownloadContentFrom(writer.ctx, grant, writer.offset, func(start int64) (io.Writer, error) {
			if start != writer.offset {
				if err := writer.restart(); err != nil {
					return nil, err
				}
			}
			return writer, nil
		})
		if err != nil {
			if writer.ctx.Err() == nil && writer.offset < size {
				if flushError := writer.flush(); flushError != nil {
					return flushError
				}
			}
			return err
		}
	}

	// 3. 整文件包括重启前的前缀。核对大小与摘要后才保存完成偏移。
	if _, err := writer.file.Seek(0, io.SeekStart); err != nil {
		return errors.New("无法读取下载临时文件")
	}
	info, err := writer.file.Stat()
	if err != nil || info.Size() != size {
		return errors.New("下载临时文件大小与授权不符")
	}
	digest, err := digestSourceSnapshot(writer.file, info)
	if err != nil {
		return err
	}
	if digest.SHA256 != writer.checkpoint.sha256 {
		if err := writer.restart(); err != nil {
			return err
		}
		return errors.New("下载正文 SHA-256 校验失败，请从头重试")
	}
	return writer.flush()
}
