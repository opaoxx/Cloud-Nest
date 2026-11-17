package service

import (
	"fmt"
	"strconv"

	"github.com/google/uuid"
)

// GenerateUploadObjectKey 为本次上传暂存尝试生成带用户前缀的对象键。
// userID 来自可信 Actor，成功结果按 stored_objects.object_key 保存并供后续存储使用。
// 失败返回空字符串和错误，用户权限由调用方的认证事务核对。
func GenerateUploadObjectKey(userID uint64) (string, error) {
	// 1. 检查用户 ID，拒绝零值。
	if userID == 0 {
		return "", fmt.Errorf("生成上传对象键需要非零用户 ID")
	}

	// 2. 生成本次暂存尝试使用的随机 UUID。
	objectUUID, err := uuid.NewRandom()
	if err != nil {
		return "", fmt.Errorf("生成上传对象键失败: %w", err)
	}

	// 3. 拼接用户前缀和 UUID，返回对象键。
	return "users/" + strconv.FormatUint(userID, 10) + "/objects/" + objectUUID.String(), nil
}
