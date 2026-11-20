package service

// BuildUploadConflictName 根据原文件名和正整数编号生成一个重名候选。
// 保留末级扩展名。无法放下编号、完整扩展名和至少一个基名字符时返回 ErrEntryNameConflict。
func BuildUploadConflictName(rawName string, suffixNumber int) (string, error) {
	return buildEntryConflictName(rawName, suffixNumber, true)
}
