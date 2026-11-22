package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// VerifiedSyncSnapshotCursor 保存已核验的绑定、下一位置和原到期时间。
// Binding.Generation 来自签名载荷，读取缓存前须与设备当前持久化代次核对。
type VerifiedSyncSnapshotCursor struct {
	Binding      SyncSnapshotCacheBinding
	NextPosition int
	ExpiresAt    time.Time
}

// SyncSnapshotCursorVerifier 使用续页专用密钥核验第 1 版快照凭据。
type SyncSnapshotCursorVerifier struct {
	signer *SyncSnapshotCursorSigner
}

// NewSyncSnapshotCursorVerifier 复用签发器的专用密钥派生和规范编码。
func NewSyncSnapshotCursorVerifier(masterKey []byte) (*SyncSnapshotCursorVerifier, error) {
	signer, err := NewSyncSnapshotCursorSigner(masterKey)
	if err != nil {
		return nil, err
	}
	return &SyncSnapshotCursorVerifier{signer: signer}, nil
}

// Verify 用已认证的账号、设备和当前路径的快照 ID 校验续页凭据。
func (verifier *SyncSnapshotCursorVerifier) Verify(userID, deviceID uint64, snapshotID uuid.UUID, cursor string, now time.Time) (*VerifiedSyncSnapshotCursor, error) {
	// 1. 检查长度及格式，先校验 HMAC，再解析载荷。
	if verifier == nil || verifier.signer == nil || len(verifier.signer.signingKey) != sha256.Size || now.IsZero() {
		return nil, fmt.Errorf("同步快照续页校验需要已初始化的密钥和当前时间")
	}
	if userID == 0 || deviceID == 0 || snapshotID.Version() != 4 || snapshotID.Variant() != uuid.RFC4122 {
		return nil, domain.ErrInvalidSyncSnapshotCursor
	}
	payloadBytes, valid := readSyncSnapshotTokenPayload(verifier.signer.signingKey, cursor)
	if !valid {
		return nil, domain.ErrInvalidSyncSnapshotCursor
	}

	// 2. 核对版本、身份和路径，再重建规范凭据，拒绝额外字段和非规范编码。
	var payload syncSnapshotCursorPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return nil, domain.ErrInvalidSyncSnapshotCursor
	}
	if payload.Version != syncSnapshotCursorVersion || payload.SnapshotID != snapshotID.String() ||
		payload.UserID != strconv.FormatUint(userID, 10) || payload.DeviceID != strconv.FormatUint(deviceID, 10) {
		return nil, domain.ErrInvalidSyncSnapshotCursor
	}
	generation, err := hex.DecodeString(payload.Generation)
	if err != nil || len(generation) != len(domain.SyncCursorGeneration{}) {
		return nil, domain.ErrInvalidSyncSnapshotCursor
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, payload.ExpiresAt)
	if err != nil {
		return nil, domain.ErrInvalidSyncSnapshotCursor
	}
	binding := SyncSnapshotCacheBinding{
		SnapshotID: snapshotID, UserID: userID, DeviceID: deviceID, Generation: domain.SyncCursorGeneration(generation), PageSize: payload.PageSize,
	}
	canonical, err := verifier.signer.Sign(binding, payload.NextPosition, expiresAt)
	if err != nil || canonical != cursor {
		return nil, domain.ErrInvalidSyncSnapshotCursor
	}

	// 3. 到期时刻含等号。成功时只交付已经核验的数据。
	if !now.Before(expiresAt) {
		return nil, domain.ErrSyncSnapshotExpired
	}
	return &VerifiedSyncSnapshotCursor{Binding: binding, NextPosition: payload.NextPosition, ExpiresAt: expiresAt.UTC()}, nil
}
