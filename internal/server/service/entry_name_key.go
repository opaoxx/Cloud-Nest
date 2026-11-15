package service

import (
	"unicode/utf8"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// maxEntryNameKeyBytes 对应 file_entries.name_key 的 VARBINARY(768) 容量。
const maxEntryNameKeyBytes = 768

// BuildEntryNameKey 校验普通目录项名称并生成用于二进制比较的 UTF-8 名称键。
// 成功结果由调用者独立持有，失败返回 nil 与名称领域错误。
func BuildEntryNameKey(rawName string) ([]byte, error) {
	// 1. 复用名称校验，取得有效展示名。
	name, err := ValidateEntryName(rawName)
	if err != nil {
		return nil, err
	}

	// 2. 按 NFC → Unicode case folding → NFC 生成判重文本。
	normalizedName := normalizeEntryNameNFC(name)
	foldedName := cases.Fold().String(normalizedName)
	keyText := normalizeEntryNameNFC(foldedName)

	// 3. 检查 UTF-8 字节长度，返回独立的名称键。
	if len(keyText) > maxEntryNameKeyBytes {
		return nil, domain.ErrInvalidEntryName
	}
	return []byte(keyText), nil
}

// normalizeEntryNameNFC 对已校验的有界名称执行规范分解、排序与组合。
// 单字符分解与双字符组合复用 norm 的 Unicode 表，保留输入中原有的 CGJ。
// 整段排序覆盖长附加符序列，避免整串流安全处理插入 CGJ 后阻挡规范重排。
func normalizeEntryNameNFC(name string) string {
	// 1. 逐字符规范分解，按组合类稳定排列同一段内的附加符。
	type decomposedCharacter struct {
		value rune
		class uint8
	}
	characters := make([]decomposedCharacter, 0, utf8.RuneCountInString(name))
	for _, character := range name {
		for _, decomposed := range norm.NFD.String(string(character)) {
			class := norm.NFD.PropertiesString(string(decomposed)).CCC()
			characters = append(characters, decomposedCharacter{value: decomposed, class: class})
			// 类为 0 的字符划分段落，同类附加符保持原有顺序。
			for position := len(characters) - 1; position > 0 && class != 0 && characters[position-1].class > class; position-- {
				characters[position], characters[position-1] = characters[position-1], characters[position]
			}
		}
	}

	// 2. 按阻挡规则组合起始字符与后续字符。
	composed := make([]rune, 0, len(characters))
	starterPosition := -1
	previousClass := uint8(0)
	for _, character := range characters {
		if starterPosition >= 0 && (previousClass == 0 || previousClass < character.class) {
			pair := string(composed[starterPosition]) + string(character.value)
			combined := norm.NFC.String(pair)
			if utf8.RuneCountInString(combined) == 1 {
				composed[starterPosition], _ = utf8.DecodeRuneInString(combined)
				// 当前字符已经合成到起始字符，后续阻挡仍取最近保留的组合类。
				continue
			}
		}
		if character.class == 0 {
			starterPosition = len(composed)
		}
		composed = append(composed, character.value)
		previousClass = character.class
	}

	// 3. 返回 NFC 文本。
	return string(composed)
}
