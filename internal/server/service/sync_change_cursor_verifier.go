package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// SyncChangeCursorVerifier 使用增量拉取专用密钥校验第 1 版游标。
type SyncChangeCursorVerifier struct {
	signer *SyncChangeCursorSigner
}

// NewSyncChangeCursorVerifier 复用签发器的密钥派生和规范编码。
func NewSyncChangeCursorVerifier(masterKey []byte) (*SyncChangeCursorVerifier, error) {
	signer, err := NewSyncChangeCursorSigner(masterKey)
	if err != nil {
		return nil, err
	}
	return &SyncChangeCursorVerifier{signer: signer}, nil
}

// Verify 用已认证的账号和设备校验游标。读取前仍须核对当前代次和日志范围。
func (verifier *SyncChangeCursorVerifier) Verify(userID, deviceID uint64, cursor string) (*domain.SyncChangeCursorPosition, error) {
	// 1. 用增量拉取专用密钥验签，通过后才解码载荷。
	if verifier == nil || verifier.signer == nil || len(verifier.signer.signingKey) != sha256.Size {
		return nil, fmt.Errorf("增量拉取游标校验需要已初始化的密钥")
	}
	if userID == 0 || deviceID == 0 {
		return nil, domain.ErrInvalidSyncChangeCursor
	}
	payloadBytes, valid := readSyncSnapshotTokenPayload(verifier.signer.signingKey, cursor)
	if !valid {
		return nil, domain.ErrInvalidSyncChangeCursor
	}

	// 2. 核对版本和身份绑定，还原完整代次和序号。
	var payload syncChangeCursorPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil || payload.Version != syncChangeCursorVersion ||
		payload.UserID != strconv.FormatUint(userID, 10) || payload.DeviceID != strconv.FormatUint(deviceID, 10) {
		return nil, domain.ErrInvalidSyncChangeCursor
	}
	generation, err := hex.DecodeString(payload.Generation)
	if err != nil || len(generation) != len(domain.SyncCursorGeneration{}) {
		return nil, domain.ErrInvalidSyncChangeCursor
	}
	sequence, err := strconv.ParseUint(payload.Sequence, 10, 64)
	if err != nil {
		return nil, domain.ErrInvalidSyncChangeCursor
	}
	position := domain.SyncChangeCursorPosition{
		UserID: userID, DeviceID: deviceID, Generation: domain.SyncCursorGeneration(generation), Sequence: sequence,
	}

	// 3. 按签发规则重建游标，原文一致才返回位置。
	canonical, err := verifier.signer.Sign(position)
	if err != nil || canonical != cursor {
		return nil, domain.ErrInvalidSyncChangeCursor
	}
	return &position, nil
}
