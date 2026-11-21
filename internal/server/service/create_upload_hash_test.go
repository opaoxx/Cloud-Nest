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

func TestBuildCreateUploadRequestHashVectors(t *testing.T) {
	for _, fixture := range []struct {
		name, rawName, digestHex, encoded, wantHex string
		parentID, size                             uint64
	}{
		{name: "trimmed name", parentID: 11, rawName: " 　学习资料.bin  ", size: 2048, digestHex: "",
			encoded: "{\"operation\":\"v1.uploads.create\",\"parent_id\":\"11\",\"name\":\"学习资料.bin\",\"size_bytes\":\"2048\",\"sha256\":null}",
			wantHex: "3cfa568a70bc419e47cc732618cb5f6b35517b10e2c936723ffcfc42a75b6778"},
		{name: "zero bytes", parentID: 11, rawName: "empty.bin", size: 0, digestHex: "",
			encoded: "{\"operation\":\"v1.uploads.create\",\"parent_id\":\"11\",\"name\":\"empty.bin\",\"size_bytes\":\"0\",\"sha256\":null}",
			wantHex: "2dd801e4eb39f3c303309cbe1c08025ad3ad0f4f6eefb45ee0ff34d480726135"},
		{name: "zero digest", parentID: 11, rawName: "empty.bin", size: 0, digestHex: "0000000000000000000000000000000000000000000000000000000000000000",
			encoded: "{\"operation\":\"v1.uploads.create\",\"parent_id\":\"11\",\"name\":\"empty.bin\",\"size_bytes\":\"0\",\"sha256\":\"0000000000000000000000000000000000000000000000000000000000000000\"}",
			wantHex: "f551e14be5c879d1ae2970c0635bf8e0f6816b4fa1ac129df90f7e4980932545"},
		{name: "binary digest", parentID: 11, rawName: "file.bin", size: 2048, digestHex: "abababababababababababababababababababababababababababababababab",
			encoded: "{\"operation\":\"v1.uploads.create\",\"parent_id\":\"11\",\"name\":\"file.bin\",\"size_bytes\":\"2048\",\"sha256\":\"abababababababababababababababababababababababababababababababab\"}",
			wantHex: "de97ca440540f99563e03879a0aaa8990cf9604a1519ba0038afdbda13f5dc44"},
		{name: "maximum parent and size", parentID: 18446744073709551615, rawName: "资料.bin", size: 16777216, digestHex: "",
			encoded: "{\"operation\":\"v1.uploads.create\",\"parent_id\":\"18446744073709551615\",\"name\":\"资料.bin\",\"size_bytes\":\"16777216\",\"sha256\":null}",
			wantHex: "318f29873b9d9610a07daf3d66a53730db69ab44161cfd344e5fceb24ccab915"},
		{name: "html escaping", parentID: 11, rawName: "A&B + '引用'", size: 1, digestHex: "",
			encoded: "{\"operation\":\"v1.uploads.create\",\"parent_id\":\"11\",\"name\":\"A\\u0026B + '引用'\",\"size_bytes\":\"1\",\"sha256\":null}",
			wantHex: "448c01da02af6ade5860d8c20fe2350b6127f3ef4d8bf73641d965f3194cffab"},
		{name: "unicode separators", parentID: 11, rawName: "a\u2028b\u2029c", size: 1, digestHex: "",
			encoded: "{\"operation\":\"v1.uploads.create\",\"parent_id\":\"11\",\"name\":\"a\\u2028b\\u2029c\",\"size_bytes\":\"1\",\"sha256\":null}",
			wantHex: "86138b6a4df4a3caaf7403e869b613a935908ff9b65745ce370017aef2e2260d"},
		{name: "decomposed display", parentID: 11, rawName: "Café", size: 1, digestHex: "",
			encoded: "{\"operation\":\"v1.uploads.create\",\"parent_id\":\"11\",\"name\":\"Café\",\"size_bytes\":\"1\",\"sha256\":null}",
			wantHex: "cedf56560e3eac629cfa0614b85551497f3375673c093e8756ece94ef544e900"},
		{name: "composed display", parentID: 11, rawName: "Café", size: 1, digestHex: "",
			encoded: "{\"operation\":\"v1.uploads.create\",\"parent_id\":\"11\",\"name\":\"Café\",\"size_bytes\":\"1\",\"sha256\":null}",
			wantHex: "b9b563821fddfdf30fc25e81d589c2bfef3e4a1d0a3aa3ee5969be79b47450b6"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			// 1. 独立 Python 编码和 SHA-256 固定向量约束协议字节。
			input := domain.CreateUploadInput{ParentID: fixture.parentID, Name: fixture.rawName, SizeBytes: fixture.size}
			if fixture.digestHex != "" {
				decoded, err := hex.DecodeString(fixture.digestHex)
				if err != nil {
					t.Fatal(err)
				}
				input.ExpectedSHA256 = new([sha256.Size]byte)
				copy(input.ExpectedSHA256[:], decoded)
			}
			original := input
			var originalDigest [sha256.Size]byte
			if input.ExpectedSHA256 != nil {
				originalDigest = *input.ExpectedSHA256
			}
			hash, err := BuildCreateUploadRequestHash(input)
			if err != nil || hex.EncodeToString(hash[:]) != fixture.wantHex || hash != sha256.Sum256([]byte(fixture.encoded)) {
				t.Fatal("上传请求固定编码或摘要错误", err)
			}
			compatible, err := BuildCreateUploadRequestHashWithMultipart(input)
			if err != nil || compatible != hash {
				t.Fatal("两种模式的入口改变了 SINGLE 历史摘要", err)
			}
			// 2. 重复计算稳定，返回数组独立，来源声明及摘要保持原值。
			repeated, err := BuildCreateUploadRequestHash(input)
			if err != nil || repeated != hash || input != original || (input.ExpectedSHA256 != nil && *input.ExpectedSHA256 != originalDigest) {
				t.Fatal("重复计算或输入保护错误", err)
			}
			hash[0] ^= 0xff
			if hex.EncodeToString(repeated[:]) != fixture.wantHex {
				t.Fatal("返回摘要相互影响")
			}
		})
	}
}

func TestBuildCreateUploadRequestHashWithMultipartVector(t *testing.T) {
	// 1. 锁定字段顺序、操作范围、上传模式、整数格式及 JSON 转义。
	const wantHex = "23ad2885034aed6f9b9b500a310877aa46a5e62997266198812718e3401bd0cc"
	encoded := `{"operation":"v1.uploads.create","upload_mode":"MULTIPART","parent_id":"18446744073709551615","name":"A\u0026B.bin","size_bytes":"16777217","sha256":"` +
		strings.Repeat("ab", sha256.Size) + `"}`
	var digest [sha256.Size]byte
	for index := range digest {
		digest[index] = 0xab
	}
	input := domain.CreateUploadInput{
		ParentID: ^uint64(0), Name: "\u3000 A&B.bin \u00a0", SizeBytes: 16777217, ExpectedSHA256: &digest,
	}
	hash, err := BuildCreateUploadRequestHashWithMultipart(input)
	if err != nil || hex.EncodeToString(hash[:]) != wantHex || hash != sha256.Sum256([]byte(encoded)) {
		t.Fatalf("Multipart 固定向量不符: %x, %v", hash, err)
	}
	if input.Name != "\u3000 A&B.bin \u00a0" || digest[0] != 0xab {
		t.Fatal("摘要计算修改了来源")
	}

	// 2. 同一规范化名称与同一文件摘要可以重新计算历史指纹。
	input.Name = "A&B.bin"
	repeated, err := BuildCreateUploadRequestHashWithMultipart(input)
	if err != nil || repeated != hash {
		t.Fatal("相同 Multipart 声明的摘要不稳定", err)
	}
}

func TestBuildCreateUploadRequestHashWithMultipartDifferences(t *testing.T) {
	digest := sha256.Sum256([]byte("fixture"))
	otherDigest := sha256.Sum256([]byte("updated fixture"))
	input := domain.CreateUploadInput{ParentID: 11, Name: "File.bin", SizeBytes: 16777217, ExpectedSHA256: &digest}
	hash, err := BuildCreateUploadRequestHashWithMultipart(input)
	if err != nil {
		t.Fatal(err)
	}
	record, now := createUploadReplayFixture(t)
	record.RequestHash = hash
	record.ResponseBody = []byte(`{"task":{"id":"42"}}`)
	if err := ValidateCreateUploadReplay(record, record.UserID, record.IdempotencyKey, hash, now); err != nil {
		t.Fatal("相同声明不满足已有的幂等比较", err)
	}
	for _, testCase := range []struct {
		name   string
		change func(*domain.CreateUploadInput)
	}{
		{"parent", func(input *domain.CreateUploadInput) { input.ParentID++ }},
		{"name", func(input *domain.CreateUploadInput) { input.Name = "file.bin" }},
		{"size", func(input *domain.CreateUploadInput) { input.SizeBytes++ }},
		{"file digest", func(input *domain.CreateUploadInput) { input.ExpectedSHA256 = &otherDigest }},
		{"switch to single", func(input *domain.CreateUploadInput) { input.SizeBytes = domain.MaxSingleUploadSizeBytes }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			changed := input
			testCase.change(&changed)
			next, err := BuildCreateUploadRequestHashWithMultipart(changed)
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateCreateUploadReplay(record, record.UserID, record.IdempotencyKey, next, now); !errors.Is(err, domain.ErrIdempotencyRequestConflict) {
				t.Fatalf("同键不同声明未识别为冲突: %v", err)
			}
		})
	}
}

func TestBuildCreateUploadRequestHashWithMultipartValidation(t *testing.T) {
	input := domain.CreateUploadInput{Name: "invalid/path", SizeBytes: 16777217}
	for _, want := range []error{domain.ErrInvalidUploadParentID, domain.ErrInvalidEntryName, domain.ErrMultipartUploadSHA256Required} {
		hash, err := BuildCreateUploadRequestHashWithMultipart(input)
		if hash != [sha256.Size]byte{} || !errors.Is(err, want) {
			t.Fatalf("无效声明返回 %x, %v，预期 %v", hash, err, want)
		}
		switch want {
		case domain.ErrInvalidUploadParentID:
			input.ParentID = 11
		case domain.ErrInvalidEntryName:
			input.Name = "fixture.bin"
		}
	}

	// 历史声明的指纹与当前存储上限无关，超限仍由新建计划检查。
	input.ExpectedSHA256 = new([sha256.Size]byte)
	input.SizeBytes = 9007199254740992
	first, firstErr := BuildCreateUploadRequestHashWithMultipart(input)
	input.SizeBytes++
	second, secondErr := BuildCreateUploadRequestHashWithMultipart(input)
	if firstErr != nil || secondErr != nil || first == second {
		t.Fatalf("相邻的大尺寸声明无法区分: %v %v", firstErr, secondErr)
	}
}

func TestBuildCreateUploadRequestHashDifferences(t *testing.T) {
	zeroDigest := [sha256.Size]byte{}
	otherDigest := sha256.Sum256([]byte("fixture-content"))
	for _, fixture := range []struct {
		name        string
		first, next domain.CreateUploadInput
	}{
		{name: "parent", first: domain.CreateUploadInput{ParentID: 11, Name: "资料"}, next: domain.CreateUploadInput{ParentID: 12, Name: "资料"}},
		{name: "size", first: domain.CreateUploadInput{ParentID: 11, Name: "资料"}, next: domain.CreateUploadInput{ParentID: 11, Name: "资料", SizeBytes: 1}},
		{name: "display", first: domain.CreateUploadInput{ParentID: 11, Name: "资料"}, next: domain.CreateUploadInput{ParentID: 11, Name: "新资料"}},
		{name: "case", first: domain.CreateUploadInput{ParentID: 11, Name: "Notes"}, next: domain.CreateUploadInput{ParentID: 11, Name: "NOTES"}},
		{name: "unicode display", first: domain.CreateUploadInput{ParentID: 11, Name: "Café"}, next: domain.CreateUploadInput{ParentID: 11, Name: "Cafe\u0301"}},
		{name: "fold equivalent", first: domain.CreateUploadInput{ParentID: 11, Name: "Straße"}, next: domain.CreateUploadInput{ParentID: 11, Name: "STRASSE"}},
		{name: "absent and zero digest", first: domain.CreateUploadInput{ParentID: 11, Name: "资料"}, next: domain.CreateUploadInput{ParentID: 11, Name: "资料", ExpectedSHA256: &zeroDigest}},
		{name: "digest value", first: domain.CreateUploadInput{ParentID: 11, Name: "资料", ExpectedSHA256: &zeroDigest}, next: domain.CreateUploadInput{ParentID: 11, Name: "资料", ExpectedSHA256: &otherDigest}},
		{name: "javascript precision", first: domain.CreateUploadInput{ParentID: 9007199254740992, Name: "资料"}, next: domain.CreateUploadInput{ParentID: 9007199254740993, Name: "资料"}},
		{name: "unsigned precision", first: domain.CreateUploadInput{ParentID: ^uint64(0) - 1, Name: "资料"}, next: domain.CreateUploadInput{ParentID: ^uint64(0), Name: "资料"}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			first, firstErr := BuildCreateUploadRequestHash(fixture.first)
			next, nextErr := BuildCreateUploadRequestHash(fixture.next)
			if firstErr != nil || nextErr != nil || first == next {
				t.Fatal("参数差异未参与请求比较", firstErr, nextErr)
			}
		})
	}
}

func TestBuildCreateUploadRequestHashFailures(t *testing.T) {
	for _, fixture := range []struct {
		name  string
		input domain.CreateUploadInput
		want  error
	}{
		{name: "zero parent", input: domain.CreateUploadInput{Name: "资料"}, want: domain.ErrInvalidUploadParentID},
		{name: "parent checked first", input: domain.CreateUploadInput{SizeBytes: ^uint64(0)}, want: domain.ErrInvalidUploadParentID},
		{name: "empty name", input: domain.CreateUploadInput{ParentID: 11}, want: domain.ErrInvalidEntryName},
		{name: "blank name", input: domain.CreateUploadInput{ParentID: 11, Name: " \u3000 "}, want: domain.ErrInvalidEntryName},
		{name: "path", input: domain.CreateUploadInput{ParentID: 11, Name: "private/name"}, want: domain.ErrInvalidEntryName},
		{name: "reserved device", input: domain.CreateUploadInput{ParentID: 11, Name: "CON.txt"}, want: domain.ErrInvalidEntryName},
		{name: "control", input: domain.CreateUploadInput{ParentID: 11, Name: "private\n"}, want: domain.ErrInvalidEntryName},
		{name: "bad UTF8", input: domain.CreateUploadInput{ParentID: 11, Name: "private\xff"}, want: domain.ErrInvalidEntryName},
		{name: "long UTF16", input: domain.CreateUploadInput{ParentID: 11, Name: strings.Repeat("🌱", 61)}, want: domain.ErrInvalidEntryName},
		{name: "trailing dot", input: domain.CreateUploadInput{ParentID: 11, Name: "private."}, want: domain.ErrInvalidEntryName},
		{name: "above limit", input: domain.CreateUploadInput{ParentID: 11, Name: "资料", SizeBytes: domain.MaxSingleUploadSizeBytes + 1}, want: domain.ErrUploadFileTooLarge},
		{name: "maximum size", input: domain.CreateUploadInput{ParentID: 11, Name: "资料", SizeBytes: ^uint64(0)}, want: domain.ErrUploadFileTooLarge},
		{name: "name before size", input: domain.CreateUploadInput{ParentID: 11, Name: "private/name", SizeBytes: ^uint64(0)}, want: domain.ErrInvalidEntryName},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			hash, err := BuildCreateUploadRequestHash(fixture.input)
			if hash != [sha256.Size]byte{} || !errors.Is(err, fixture.want) || strings.Contains(err.Error(), "private") {
				t.Fatal("失败返回或错误顺序异常", err)
			}
		})
	}
}

func TestBuildCreateUploadRequestHashBounds(t *testing.T) {
	for _, name := range []string{strings.Repeat("a", 120), strings.Repeat("中", 120), strings.Repeat("🌱", 60), strings.Repeat("\u0f73", 120)} {
		for _, size := range []uint64{0, domain.MaxSingleUploadSizeBytes} {
			input := domain.CreateUploadInput{ParentID: ^uint64(0), Name: name, SizeBytes: size}
			plain, err := BuildCreateUploadRequestHash(input)
			if err != nil {
				t.Fatal("有效边界被拒绝", err)
			}
			input.Name = "\u3000" + name + "\u00a0"
			padded, err := BuildCreateUploadRequestHash(input)
			if err != nil || plain != padded || plain == [sha256.Size]byte{} {
				t.Fatal("首尾空白归一化错误", err)
			}
		}
	}
	if domain.CreateUploadOperation != "v1.uploads.create" || len(domain.CreateUploadOperation) > 64 {
		t.Fatal("操作名协议或容量错误")
	}
	input := domain.CreateUploadInput{ParentID: 11, Name: "资料", SizeBytes: 1}
	hash, err := BuildCreateUploadRequestHash(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{domain.CreateDirectoryOperation, domain.RenameEntryOperation, domain.MoveEntryOperation} {
		encoded := `{"operation":"` + operation + `","parent_id":"11","name":"资料","size_bytes":"1","sha256":null}`
		if hash == sha256.Sum256([]byte(encoded)) {
			t.Fatal("摘要未绑定上传创建操作")
		}
	}
}

func TestBuildCreateUploadRequestHashConcurrent(t *testing.T) {
	digest := sha256.Sum256([]byte("shared fixture"))
	input := domain.CreateUploadInput{ParentID: 11, Name: " 资料.bin ", SizeBytes: 2048, ExpectedSHA256: &digest}
	expected, err := BuildCreateUploadRequestHash(input)
	if err != nil {
		t.Fatal(err)
	}
	originalDigest := digest
	var workers sync.WaitGroup
	for worker := 0; worker < 32; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for attempt := 0; attempt < 20; attempt++ {
				hash, err := BuildCreateUploadRequestHash(input)
				if err != nil || hash != expected {
					t.Error("并发摘要不稳定", err)
				}
			}
		}()
	}
	workers.Wait()
	if digest != originalDigest || input.Name != " 资料.bin " {
		t.Fatal("并发调用修改来源")
	}
}
