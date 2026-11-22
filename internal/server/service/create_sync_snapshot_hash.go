package service

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

// BuildCreateSyncSnapshotRequestHash 绑定已认证的设备及规范化后的页大小。
// 用户与幂等键由记录的查找范围约束。校验失败时返回零值摘要。
func BuildCreateSyncSnapshotRequestHash(deviceID uint64, pageSize int) ([sha256.Size]byte, error) {
	// 1. 校验设备 ID 非零，页大小在 1～100 之间。
	if deviceID == 0 || pageSize < 1 || pageSize > 100 {
		return [sha256.Size]byte{}, fmt.Errorf("生成快照创建摘要需要非零设备 ID 和 1～100 的页大小")
	}

	// 2. 将版本化操作名、设备 ID 和页大小按固定顺序编码。
	request := struct {
		Operation string `json:"operation"`
		DeviceID  string `json:"device_id"`
		PageSize  int    `json:"page_size"`
	}{
		Operation: domain.CreateSyncSnapshotOperation,
		DeviceID:  strconv.FormatUint(deviceID, 10),
		PageSize:  pageSize,
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("编码快照创建请求摘要失败: %w", err)
	}

	// 3. 对编码后的参数计算 SHA-256。
	return sha256.Sum256(encoded), nil
}
