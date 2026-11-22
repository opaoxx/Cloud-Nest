package domain

import (
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// ParseUploadObjectKey 从应用生成的规范对象键中读取账号 ID。
// 该结果用于限制扫描范围，账号和存储记录的归属仍以仓储复核为准。
func ParseUploadObjectKey(key string) (uint64, bool) {
	// 1. 拆开账号目录和对象标识，校验规范的非零账号 ID。
	path, hasPrefix := strings.CutPrefix(key, "users/")
	owner, object, hasObject := strings.Cut(path, "/objects/")
	if !hasPrefix || !hasObject {
		return 0, false
	}
	userID, err := strconv.ParseUint(owner, 10, 64)
	if err != nil || userID == 0 || strconv.FormatUint(userID, 10) != owner {
		return 0, false
	}

	// 2. 只接受随机 UUID 的规范文本。
	objectID, err := uuid.Parse(object)
	if err != nil || objectID.Version() != 4 || objectID.Variant() != uuid.RFC4122 || objectID.String() != object {
		return 0, false
	}
	return userID, true
}
