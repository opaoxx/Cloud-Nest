package service

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func TestBuildCreateDirectoryRequestHashVectors(t *testing.T) {
	green, empty, sand := "green", "", "sand"
	for _, testCase := range []struct {
		name, rawName, encoded, wantHex string
		parentID                        uint64
		color                           *string
	}{
		{name: "null color and trimmed name", parentID: 11, rawName: " \u3000学习资料\u00a0 ",
			encoded: `{"operation":"v1.directories.create","parent_id":"11","name":"学习资料","color_key":null}`,
			wantHex: "0122bc7c0d8542569bb9e0b564aaaf4415cac68aa137a3e34e31acf63ee88b36"},
		{name: "green color", parentID: 11, rawName: "学习资料", color: &green,
			encoded: `{"operation":"v1.directories.create","parent_id":"11","name":"学习资料","color_key":"green"}`,
			wantHex: "260769a071cbfc631b1acf751ef5b9973f2219ce881685b7f37718f9339f6084"},
		{name: "empty color", parentID: 11, rawName: "学习资料", color: &empty,
			encoded: `{"operation":"v1.directories.create","parent_id":"11","name":"学习资料","color_key":""}`,
			wantHex: "8250a747318fa56dbf6c8648dcf23f0a682ce82bf3f7aa642db35d35c8ca31ec"},
		{name: "maximum parent", parentID: ^uint64(0), rawName: "目录",
			encoded: `{"operation":"v1.directories.create","parent_id":"18446744073709551615","name":"目录","color_key":null}`,
			wantHex: "341eda3a37368cccb8751ccef5145061c73ac92c252940e45b37162925522b75"},
		{name: "html escaping", parentID: 11, rawName: "A&B + '引用'",
			encoded: `{"operation":"v1.directories.create","parent_id":"11","name":"A\u0026B + '引用'","color_key":null}`,
			wantHex: "b5762aa87fa53982de86b82f343cfccfd0d12204b49e71e96b2027292e60f9d0"},
		{name: "unicode separators", parentID: 11, rawName: "a\u2028b\u2029c", color: &sand,
			encoded: `{"operation":"v1.directories.create","parent_id":"11","name":"a\u2028b\u2029c","color_key":"sand"}`,
			wantHex: "1aa1c53bc8934a442e9279baf66dbae0c6d6f74d9122cda54aa6efb4b35407c7"},
		{name: "decomposed display", parentID: 11, rawName: "Cafe\u0301",
			encoded: "{\"operation\":\"v1.directories.create\",\"parent_id\":\"11\",\"name\":\"Cafe\u0301\",\"color_key\":null}",
			wantHex: "9b1373a2bc0cdd5037d6bfc1a66bc4bc7e0dbcc11fb531fffc8c167f26510d37"},
		{name: "composed display", parentID: 11, rawName: "Café",
			encoded: `{"operation":"v1.directories.create","parent_id":"11","name":"Café","color_key":null}`,
			wantHex: "49d8a22f3987192d101854fa7d385bc50da0053575639807b25dcc32f37652c5"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// 1. 固定向量由独立 Python JSON/SHA-256 生成，核对约定编码及字节摘要。
			hash, err := BuildCreateDirectoryRequestHash(testCase.parentID, testCase.rawName, testCase.color)
			if err != nil || len(hash) != 32 || hex.EncodeToString(hash[:]) != testCase.wantHex || hash != sha256.Sum256([]byte(testCase.encoded)) {
				t.Fatal("创建目录固定编码或摘要不一致", err)
			}
			// 2. 重复计算结果一致，每个返回数组由调用方独立持有。
			second, err := BuildCreateDirectoryRequestHash(testCase.parentID, testCase.rawName, testCase.color)
			if err != nil || second != hash {
				t.Fatal("重复生成摘要不稳定", err)
			}
			hash[0] ^= 0xff
			if hex.EncodeToString(second[:]) != testCase.wantHex {
				t.Fatal("修改一次摘要影响其他结果")
			}
		})
	}
}

func TestBuildCreateDirectoryRequestHashDifferences(t *testing.T) {
	green, upper, padded, sand, empty := "green", "Green", " green", "sand", ""
	for _, testCase := range []struct {
		name                    string
		firstParent, nextParent uint64
		firstName, nextName     string
		firstColor, nextColor   *string
	}{
		{name: "parent", firstParent: 11, nextParent: 12, firstName: "资料", nextName: "资料"},
		{name: "display name", firstParent: 11, nextParent: 11, firstName: "资料", nextName: "新资料"},
		{name: "display case", firstParent: 11, nextParent: 11, firstName: "Notes", nextName: "NOTES"},
		{name: "display unicode spelling", firstParent: 11, nextParent: 11, firstName: "Café", nextName: "Cafe\u0301"},
		{name: "fold equivalent display", firstParent: 11, nextParent: 11, firstName: "Straße", nextName: "STRASSE"},
		{name: "null and empty color", firstParent: 11, nextParent: 11, firstName: "资料", nextName: "资料", nextColor: &empty},
		{name: "color", firstParent: 11, nextParent: 11, firstName: "资料", nextName: "资料", firstColor: &green, nextColor: &sand},
		{name: "color case", firstParent: 11, nextParent: 11, firstName: "资料", nextName: "资料", firstColor: &green, nextColor: &upper},
		{name: "color whitespace", firstParent: 11, nextParent: 11, firstName: "资料", nextName: "资料", firstColor: &green, nextColor: &padded},
		{name: "javascript integer precision", firstParent: 9007199254740992, nextParent: 9007199254740993, firstName: "资料", nextName: "资料"},
		{name: "unsigned integer precision", firstParent: ^uint64(0) - 1, nextParent: ^uint64(0), firstName: "资料", nextName: "资料"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			first, firstError := BuildCreateDirectoryRequestHash(testCase.firstParent, testCase.firstName, testCase.firstColor)
			next, nextError := BuildCreateDirectoryRequestHash(testCase.nextParent, testCase.nextName, testCase.nextColor)
			if firstError != nil || nextError != nil || first == next {
				t.Fatal("业务参数改变未产生不同摘要", firstError, nextError)
			}
		})
	}
}

func TestBuildCreateDirectoryRequestHashFailures(t *testing.T) {
	longColor, invalidColor := strings.Repeat("a", 17), "private-color\n"
	for _, testCase := range []struct {
		name, rawName string
		parentID      uint64
		color         *string
		want          error
	}{
		{name: "zero parent", rawName: "资料"},
		{name: "empty name", parentID: 11, want: domain.ErrInvalidEntryName},
		{name: "blank name", parentID: 11, rawName: " \u3000 ", want: domain.ErrInvalidEntryName},
		{name: "path separator", parentID: 11, rawName: "private/name", want: domain.ErrInvalidEntryName},
		{name: "reserved device", parentID: 11, rawName: "CON.txt", want: domain.ErrInvalidEntryName},
		{name: "control", parentID: 11, rawName: "资料\n", want: domain.ErrInvalidEntryName},
		{name: "invalid utf8", parentID: 11, rawName: "private-name\xff", want: domain.ErrInvalidEntryName},
		{name: "utf16 overflow", parentID: 11, rawName: strings.Repeat("🌱", 61), want: domain.ErrInvalidEntryName},
		{name: "long color", parentID: 11, rawName: "资料", color: &longColor, want: domain.ErrInvalidDirectoryColorKey},
		{name: "invalid color", parentID: 11, rawName: "资料", color: &invalidColor, want: domain.ErrInvalidDirectoryColorKey},
		{name: "name checked before color", parentID: 11, color: &invalidColor, want: domain.ErrInvalidEntryName},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			hash, err := BuildCreateDirectoryRequestHash(testCase.parentID, testCase.rawName, testCase.color)
			if hash != [sha256.Size]byte{} || err == nil || (testCase.want != nil && !errors.Is(err, testCase.want)) || strings.Contains(err.Error(), "private-") {
				t.Fatal("无效输入未返回零值摘要及固定错误", err)
			}
		})
	}
}

func TestBuildCreateDirectoryRequestHashBounds(t *testing.T) {
	for _, name := range []string{strings.Repeat("A", 120), strings.Repeat("中", 120), strings.Repeat("🌱", 60), strings.Repeat("\u0f73", 120)} {
		plain, plainError := BuildCreateDirectoryRequestHash(1, name, nil)
		padded, paddedError := BuildCreateDirectoryRequestHash(1, "\u3000"+name+"\u00a0", nil)
		if plainError != nil || paddedError != nil || plain != padded || plain == [sha256.Size]byte{} {
			t.Fatal("有效名称长度边界或去空白摘要错误", plainError, paddedError)
		}
	}
	if domain.CreateDirectoryOperation != "v1.directories.create" || len(domain.CreateDirectoryOperation) > 64 {
		t.Fatal("版本化操作名与固定编码或数据库容量不一致")
	}
}

func TestBuildCreateDirectoryRequestHashConcurrent(t *testing.T) {
	color := "green"
	var workers sync.WaitGroup
	for worker := 0; worker < 64; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for attempt := 0; attempt < 20; attempt++ {
				hash, err := BuildCreateDirectoryRequestHash(11, " 学习资料 ", &color)
				if err != nil || hex.EncodeToString(hash[:]) != "260769a071cbfc631b1acf751ef5b9973f2219ce881685b7f37718f9339f6084" {
					t.Error("并发生成摘要不一致", err)
				}
			}
		}()
	}
	workers.Wait()
	if color != "green" {
		t.Fatal("摘要生成修改了输入颜色")
	}
}
