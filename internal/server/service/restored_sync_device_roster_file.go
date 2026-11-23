package service

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// restoredSyncDeviceRosterRow 定义离线恢复清单的文件字段。
// 用定长十六进制表示代次，避免 JSON 数组解码时补零或丢弃多余字节。
type restoredSyncDeviceRosterRow struct {
	UserID      uint64                  `json:"user_id"`
	DeviceID    uint64                  `json:"device_id"`
	Status      domain.SyncDeviceStatus `json:"status"`
	Generation  string                  `json:"generation"`
	AckSequence *uint64                 `json:"ack_sequence"`
}

// SaveRestoredSyncDeviceRoster 保存首次导入清单。父目录须已存在，
// 正式文件名已存在时保留原文件并返回可用 errors.Is 判断的 os.ErrExist。
func SaveRestoredSyncDeviceRoster(path string, imported []domain.RestoredSyncDeviceCandidate) (err error) {
	// 1. 校验清单，区分缺失清单与明确的空清单。
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("保存恢复同步设备清单需要目标路径")
	}
	if imported == nil || !validRestoredSyncDevicePage(imported, 0, len(imported)) {
		return fmt.Errorf("恢复同步设备导入清单无效: %w", domain.ErrInvalidSyncDeviceData)
	}
	rows := make([]restoredSyncDeviceRosterRow, 0, len(imported))
	for _, candidate := range imported {
		state := candidate.State
		rows = append(rows, restoredSyncDeviceRosterRow{
			UserID: candidate.UserID, DeviceID: candidate.DeviceID, Status: state.Status,
			Generation: hex.EncodeToString(state.Generation[:]), AckSequence: &state.AckSequence,
		})
	}

	// 2. 将完整内容写入同目录临时文件并同步。
	temporaryFile, err := os.CreateTemp(filepath.Dir(path), ".cloudnest-sync-roster-*")
	if err != nil {
		return fmt.Errorf("创建恢复清单临时文件失败: %w", err)
	}
	defer func() {
		if closeErr := temporaryFile.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
			err = errors.Join(err, fmt.Errorf("关闭恢复清单临时文件失败: %w", closeErr))
		}
		if removeErr := os.Remove(temporaryFile.Name()); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("清理恢复清单临时文件失败: %w", removeErr))
		}
	}()
	if err := json.NewEncoder(temporaryFile).Encode(rows); err != nil {
		return fmt.Errorf("写入恢复清单失败: %w", err)
	}
	if err := temporaryFile.Sync(); err != nil {
		return fmt.Errorf("同步恢复清单失败: %w", err)
	}
	if err := temporaryFile.Close(); err != nil {
		return fmt.Errorf("关闭恢复清单失败: %w", err)
	}

	// 3. 创建正式文件名，已有同名文件时保留原件。
	if err := os.Link(temporaryFile.Name(), path); err != nil {
		return fmt.Errorf("发布恢复清单失败: %w", err)
	}
	return nil
}

// LoadRestoredSyncDeviceRoster 读取此前保存的原始清单。空数组返回明确的空清单，
// 文件缺失或解析失败时返回 nil 和错误，调用方可据此停止恢复。
func LoadRestoredSyncDeviceRoster(path string) ([]domain.RestoredSyncDeviceCandidate, error) {
	// 1. 打开正式文件，只接受恰好一个非 null 的 JSON 数组。
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("打开恢复同步设备清单失败: %w", err)
	}
	defer file.Close()

	var rows []restoredSyncDeviceRosterRow
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&rows); err != nil {
		return nil, fmt.Errorf("解析恢复同步设备清单失败: %w", err)
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF || rows == nil {
		return nil, fmt.Errorf("恢复同步设备清单必须是单一 JSON 数组: %w", domain.ErrInvalidSyncDeviceData)
	}

	// 2. 恢复完整代次与确认位置，再按换代流程共用的规则检查顺序和状态。
	imported := make([]domain.RestoredSyncDeviceCandidate, 0, len(rows))
	for _, row := range rows {
		var generation domain.SyncCursorGeneration
		if len(row.Generation) != hex.EncodedLen(len(generation)) || row.AckSequence == nil {
			return nil, fmt.Errorf("设备 %d 的代次或确认位置缺失或无效: %w", row.DeviceID, domain.ErrInvalidSyncDeviceData)
		}
		if _, err := hex.Decode(generation[:], []byte(row.Generation)); err != nil {
			return nil, fmt.Errorf("设备 %d 的代次编码无效: %w", row.DeviceID, domain.ErrInvalidSyncDeviceData)
		}
		imported = append(imported, domain.RestoredSyncDeviceCandidate{
			UserID: row.UserID, DeviceID: row.DeviceID,
			State: domain.SyncDeviceState{Status: row.Status, Generation: generation, AckSequence: *row.AckSequence},
		})
	}
	if !validRestoredSyncDevicePage(imported, 0, len(imported)) {
		return nil, fmt.Errorf("恢复同步设备导入清单无效: %w", domain.ErrInvalidSyncDeviceData)
	}
	return imported, nil
}
