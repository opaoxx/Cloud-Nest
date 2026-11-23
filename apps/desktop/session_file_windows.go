package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// savedSession 保存恢复会话所需的服务地址、账号和刷新凭证。
type savedSession struct {
	Address      string `json:"address"`
	Username     string `json:"username"`
	RefreshToken string `json:"refresh_token"`
}

type sessionFile struct {
	path           string
	installationID string
}

// lock 串行化同一安装的登录与恢复，覆盖读取、请求和更新文件。
func (file sessionFile) lock() (*os.File, error) {
	lock, err := os.OpenFile(file.path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, errors.New("无法打开会话保存目录")
	}
	var offset windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(lock.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &offset); err != nil {
		lock.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, errors.New("另一个 CloudNest 窗口正在登录或恢复，请稍后重试")
		}
		return nil, errors.New("无法锁定本机的会话记录")
	}
	return lock, nil
}

func (file sessionFile) read() (savedSession, error) {
	encryptedFile, err := os.Open(file.path)
	if err != nil {
		return savedSession{}, err
	}
	defer encryptedFile.Close()
	const maxSessionBytes = 64 * 1024
	encrypted, err := io.ReadAll(io.LimitReader(encryptedFile, maxSessionBytes+1))
	if err != nil || len(encrypted) == 0 || len(encrypted) > maxSessionBytes {
		return savedSession{}, errors.New("无法读取保存的会话，请重新登录")
	}

	// 1. 使用当前 Windows 用户与安装标识解开本机记录。
	input := newDataBlob(encrypted)
	entropy := newDataBlob([]byte(file.installationID))
	var output windows.DataBlob
	if err := windows.CryptUnprotectData(&input, nil, &entropy, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); err != nil {
		return savedSession{}, errors.New("无法解开保存的会话，请重新登录")
	}
	defer windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(output.Data))))

	// 2. 读出恢复请求需要的字段，清理系统返回的明文缓冲区。
	plaintext := unsafe.Slice(output.Data, output.Size)
	defer clear(plaintext)
	var record savedSession
	if err := json.Unmarshal(plaintext, &record); err != nil || record.Address == "" || record.Username == "" || record.RefreshToken == "" {
		return savedSession{}, errors.New("保存的会话记录不完整，请重新登录")
	}
	return record, nil
}

func (file sessionFile) write(record savedSession) error {
	// 1. 使用 DPAPI 保护完整记录，磁盘只接收加密后的数据。
	plaintext, err := json.Marshal(record)
	if err != nil {
		return err
	}
	defer clear(plaintext)
	input := newDataBlob(plaintext)
	entropy := newDataBlob([]byte(file.installationID))
	var output windows.DataBlob
	if err := windows.CryptProtectData(&input, nil, &entropy, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); err != nil {
		return err
	}
	defer windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(output.Data))))

	// 2. 写完、同步并关闭同目录临时文件，再替换正式凭证文件。
	staged, err := os.CreateTemp(filepath.Dir(file.path), ".session-*")
	if err != nil {
		return err
	}
	defer os.Remove(staged.Name())
	defer staged.Close()
	if _, err := staged.Write(unsafe.Slice(output.Data, output.Size)); err != nil {
		return err
	}
	if err := staged.Sync(); err != nil {
		return err
	}
	if err := staged.Close(); err != nil {
		return err
	}
	return os.Rename(staged.Name(), file.path)
}

func newDataBlob(data []byte) windows.DataBlob {
	return windows.DataBlob{Size: uint32(len(data)), Data: unsafe.SliceData(data)}
}
