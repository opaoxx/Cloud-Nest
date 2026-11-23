package main

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/opaoxx/cloud-nest/pkg/client"
)

// UploadIntents 保存和读取当前账号在本机的上传队列。
type UploadIntents struct {
	account      *Account
	picker       *LocalFilePicker
	databasePath string
}

// UploadIntentResult 将本机队列或重新登录提示交给窗口。
type UploadIntentResult struct {
	Items         []UploadIntentView `json:"items"`
	LoginRequired string             `json:"loginRequired"`
}

// Save 以用户选中的本机文件及当前目录创建或找回一条本机记录。
func (uploads *UploadIntents) Save(ctx context.Context, parentID, creationKey string) (UploadIntentResult, error) {
	// 1. 确认当前目标与本次选择，路径只取自 Go 文件选择器。
	if !validUploadID(parentID) {
		return UploadIntentResult{}, errors.New("请先打开目标目录")
	}
	source, err := uploads.picker.selectedSource(creationKey)
	if err != nil {
		return UploadIntentResult{}, err
	}
	return uploads.load(ctx, &uploadIntentDraft{Source: source, ParentID: parentID})
}

// List 读取当前服务及账号的本机上传队列。
func (uploads *UploadIntents) List(ctx context.Context) (UploadIntentResult, error) {
	return uploads.load(ctx, nil)
}

func (uploads *UploadIntents) load(ctx context.Context, draft *uploadIntentDraft) (UploadIntentResult, error) {
	// 1. 从后端本人资料确认数值账号 ID，沿用会话续期与切换保护。
	account := uploads.account
	account.mu.Lock()
	epoch := account.sessionEpoch
	account.mu.Unlock()
	owner, err := readForAccount(ctx, account, func(address string, session client.NativeSession) (uploadOwner, error) {
		return resolveUploadOwner(ctx, address, session)
	})
	if errors.Is(err, errLoginRequired) || errors.Is(err, errRefreshOutcomeUnknown) {
		return UploadIntentResult{LoginRequired: err.Error()}, nil
	}
	if err != nil {
		return UploadIntentResult{}, err
	}

	// 2. 复核会话后在同一账户锁下完成本机写入及读取。
	account.mu.Lock()
	defer account.mu.Unlock()
	if account.sessionEpoch != epoch {
		return UploadIntentResult{}, errSessionChanged
	}
	db, err := openUploadIntents(ctx, uploads.databasePath)
	if err != nil {
		return UploadIntentResult{}, errors.New("无法打开本机上传记录，请检查应用数据目录")
	}
	defer db.Close()
	if draft != nil {
		if err := insertUploadIntent(ctx, db, owner, *draft); err != nil {
			if errors.Is(err, errUploadKeyConflict) {
				return UploadIntentResult{}, err
			}
			return UploadIntentResult{}, errors.New("无法保存上传意图，请重试")
		}
	}

	// 3. 将该账号的本机上传队列交给窗口。
	items, err := listUploadIntents(ctx, db, owner)
	if err != nil {
		return UploadIntentResult{}, errors.New("无法读取本机上传记录")
	}
	return UploadIntentResult{Items: items}, nil
}

func resolveUploadOwner(ctx context.Context, address string, session client.NativeSession) (uploadOwner, error) {
	profile, err := client.ReadCurrentProfile(ctx, address, session.AccessToken)
	if err != nil {
		return uploadOwner{}, err
	}
	if profile.Username != session.Username || !validUploadID(profile.UserID) {
		return uploadOwner{}, errors.New("无法确认上传记录所属的账号")
	}
	return uploadOwner{Address: strings.TrimSuffix(strings.TrimSpace(address), "/"), UserID: profile.UserID}, nil
}

func validUploadID(text string) bool {
	value, err := strconv.ParseUint(text, 10, 64)
	return err == nil && value != 0 && strconv.FormatUint(value, 10) == text
}
