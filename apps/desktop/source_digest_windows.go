package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// SourceFileDigest 是窗口展示的本机文件摘要。字节数以字符串传给 JavaScript，保留整数精度。
type SourceFileDigest struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes,string"`
	SHA256    string `json:"sha256"`
}

func readSourceFileDigest(ctx context.Context, path string) (*SourceFileDigest, error) {
	source, err := openSourceFile(path)
	if err != nil {
		return nil, err
	}
	defer source.Close()
	before, err := source.Stat()
	if err != nil || !before.Mode().IsRegular() {
		return nil, errors.New("无法检查所选文件，请重新选择一个普通文件")
	}
	return digestSourceSnapshot(ctx, source, before)
}

func openSourceFile(path string) (*os.File, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("无法识别所选文件的位置，请重新选择")
	}
	source, err := os.Open(path)
	if err != nil {
		return nil, errors.New("无法打开所选文件，请重新选择")
	}
	return source, nil
}

func digestSourceSnapshot(ctx context.Context, source *os.File, before os.FileInfo) (*SourceFileDigest, error) {
	// 1. 以初始大小为读取边界，分块更新摘要，并累计实际读出的字节数。
	hasher := sha256.New()
	sizeBytes, err := io.Copy(hasher, io.LimitReader(transferReader{ctx: ctx, source: source}, before.Size()))
	if err != nil {
		return nil, errors.New("无法读取所选文件的内容，请重新选择")
	}

	// 2. 复核大小和修改时间，发现文件在读取期间变化时要求重新选择。
	after, err := source.Stat()
	if err != nil {
		return nil, errors.New("无法复查所选文件，请重新选择")
	}
	if sizeBytes != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return nil, errors.New("文件在读取期间发生变化，请重新选择")
	}
	return &SourceFileDigest{
		Name:      before.Name(),
		SizeBytes: sizeBytes,
		SHA256:    hex.EncodeToString(hasher.Sum(nil)),
	}, nil
}
