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

// VerifiedSyncSnapshotBaseline 保存已核验的快照绑定、截止序号和原到期时间。
// 确认或首次拉取前，调用方须比较当前已持久化代次并复核可续读区间。
type VerifiedSyncSnapshotBaseline struct {
	SnapshotID        uuid.UUID
	UserID            uint64
	DeviceID          uint64
	Generation        domain.SyncCursorGeneration
	HighWaterSequence uint64
	ExpiresAt         time.Time
}

// SyncSnapshotBaselineVerifier 使用末页确认专用密钥校验版本 1 凭据。
type SyncSnapshotBaselineVerifier struct {
	signer *SyncSnapshotBaselineSigner
}

// NewSyncSnapshotBaselineVerifier 复用确认凭据签发器的密钥派生和编码。
func NewSyncSnapshotBaselineVerifier(masterKey []byte) (*SyncSnapshotBaselineVerifier, error) {
	signer, err := NewSyncSnapshotBaselineSigner(masterKey)
	if err != nil {
		return nil, err
	}
	return &SyncSnapshotBaselineVerifier{signer: signer}, nil
}

// Verify 用已认证的账号、设备和本次确认请求的快照 ID 核验末页确认凭据。
func (verifier *SyncSnapshotBaselineVerifier) Verify(userID, deviceID uint64, snapshotID uuid.UUID, token string, now time.Time) (*VerifiedSyncSnapshotBaseline, error) {
	return verifier.verify(userID, deviceID, &snapshotID, token, now)
}

// VerifyForPull 为首次拉取验证末页凭据，快照 ID 从已验签载荷取得。
// 调用方仍须在一致性视图中核对设备已确认该代次及其截止位置。
func (verifier *SyncSnapshotBaselineVerifier) VerifyForPull(userID, deviceID uint64, token string, now time.Time) (*VerifiedSyncSnapshotBaseline, error) {
	return verifier.verify(userID, deviceID, nil, token, now)
}

func (verifier *SyncSnapshotBaselineVerifier) verify(userID, deviceID uint64, expectedSnapshotID *uuid.UUID, token string, now time.Time) (*VerifiedSyncSnapshotBaseline, error) {
	// 1. 用末页确认专用密钥验证签名，再解析载荷。
	if verifier == nil || verifier.signer == nil || len(verifier.signer.signingKey) != sha256.Size || now.IsZero() {
		return nil, fmt.Errorf("同步快照末页确认校验需要已初始化的密钥和当前时间")
	}
	if userID == 0 || deviceID == 0 {
		return nil, domain.ErrInvalidSyncSnapshotBaselineToken
	}
	payloadBytes, valid := readSyncSnapshotTokenPayload(verifier.signer.signingKey, token)
	if !valid {
		return nil, domain.ErrInvalidSyncSnapshotBaselineToken
	}

	// 2. 核对账号、设备和载荷中的快照 ID。确认请求还需匹配路径。
	var payload syncSnapshotBaselinePayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return nil, domain.ErrInvalidSyncSnapshotBaselineToken
	}
	if payload.Version != syncSnapshotBaselineVersion ||
		payload.UserID != strconv.FormatUint(userID, 10) || payload.DeviceID != strconv.FormatUint(deviceID, 10) {
		return nil, domain.ErrInvalidSyncSnapshotBaselineToken
	}
	snapshotID, err := uuid.Parse(payload.SnapshotID)
	if err != nil || snapshotID.Version() != 4 || snapshotID.Variant() != uuid.RFC4122 || snapshotID.String() != payload.SnapshotID ||
		(expectedSnapshotID != nil && *expectedSnapshotID != snapshotID) {
		return nil, domain.ErrInvalidSyncSnapshotBaselineToken
	}

	// 3. 还原代次、序号与期限，按签发规则重建凭据并检查原期限。
	generation, err := hex.DecodeString(payload.Generation)
	if err != nil || len(generation) != len(domain.SyncCursorGeneration{}) {
		return nil, domain.ErrInvalidSyncSnapshotBaselineToken
	}
	sequence, err := strconv.ParseUint(payload.HighWaterSequence, 10, 64)
	if err != nil {
		return nil, domain.ErrInvalidSyncSnapshotBaselineToken
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, payload.ExpiresAt)
	if err != nil {
		return nil, domain.ErrInvalidSyncSnapshotBaselineToken
	}
	binding := SyncSnapshotCacheBinding{
		SnapshotID: snapshotID, UserID: userID, DeviceID: deviceID, Generation: domain.SyncCursorGeneration(generation),
	}

	canonical, err := verifier.signer.Sign(binding, sequence, expiresAt)
	if err != nil || canonical != token {
		return nil, domain.ErrInvalidSyncSnapshotBaselineToken
	}
	if !now.Before(expiresAt) {
		return nil, domain.ErrSyncSnapshotExpired
	}
	return &VerifiedSyncSnapshotBaseline{
		SnapshotID: snapshotID, UserID: userID, DeviceID: deviceID, Generation: binding.Generation,
		HighWaterSequence: sequence, ExpiresAt: expiresAt.UTC(),
	}, nil
}
