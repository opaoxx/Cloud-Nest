package service

import (
	"bytes"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/google/uuid"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
)

func checkUploadObjectKey(t *testing.T, userID uint64, key string) {
	t.Helper()
	// 1. 核对用户前缀、固定长度及对象键安全字符。
	prefix := "users/" + strconv.FormatUint(userID, 10) + "/objects/"
	if !strings.HasPrefix(key, prefix) || len(key) != len(prefix)+36 || len(key) > 255 {
		t.Fatal("对象键前缀或列长度错误")
	}
	const allowed = "abcdefghijklmnopqrstuvwxyz0123456789-/"
	for _, character := range key {
		if !strings.ContainsRune(allowed, character) {
			t.Fatal("对象键包含约定范围外字符")
		}
	}
	// 2. 核对规范 UUID v4，以及暂存对象输入的列宽与编码约束。
	uuidText := strings.TrimPrefix(key, prefix)
	objectUUID, err := uuid.Parse(uuidText)
	if err != nil || objectUUID.Version() != 4 || objectUUID.Variant() != uuid.RFC4122 || objectUUID.String() != uuidText {
		t.Fatal("对象键 UUID 格式错误", err)
	}
	input := domain.CreateStagedObjectInput{Provider: "local_minio", Bucket: "cloudnest", ObjectKey: key}
	if !input.IsValid() {
		t.Fatal("生成对象键不符合暂存对象声明")
	}
}

func TestGenerateUploadObjectKeyBounds(t *testing.T) {
	for _, userID := range []uint64{1, 7, 9007199254740993, 9223372036854775808, ^uint64(0)} {
		t.Run(strconv.FormatUint(userID, 10), func(t *testing.T) {
			key, err := GenerateUploadObjectKey(userID)
			if err != nil {
				t.Fatal("有效用户生成对象键失败", err)
			}
			checkUploadObjectKey(t, userID, key)
		})
	}
}

func TestGenerateUploadObjectKeyVectors(t *testing.T) {
	// 本测试串行使用 UUID 库的随机源测试入口，结束后恢复默认安全随机源。
	for _, fixture := range []struct {
		name     string
		entropy  []byte
		wantUUID string
	}{
		{name: "zero bytes", entropy: make([]byte, 16), wantUUID: "00000000-0000-4000-8000-000000000000"},
		{name: "all bits", entropy: bytes.Repeat([]byte{0xff}, 16), wantUUID: "ffffffff-ffff-4fff-bfff-ffffffffffff"},
		{name: "ordered bytes", entropy: []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}, wantUUID: "00010203-0405-4607-8809-0a0b0c0d0e0f"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			reader := bytes.NewReader(fixture.entropy)
			uuid.SetRand(iotest.OneByteReader(reader))
			t.Cleanup(func() { uuid.SetRand(nil) })
			key, err := GenerateUploadObjectKey(7)
			if err != nil || key != "users/7/objects/"+fixture.wantUUID || reader.Len() != 0 {
				t.Fatal("对象键固定编码或完整随机读取错误", err)
			}
			checkUploadObjectKey(t, 7, key)
		})
	}
}

func TestGenerateUploadObjectKeyFailures(t *testing.T) {
	randomError := errors.New("fixture random source failure")
	for _, fixture := range []struct {
		name   string
		userID uint64
		reader io.Reader
		want   error
	}{
		{name: "zero user checked first", reader: iotest.ErrReader(randomError)},
		{name: "random source failure", userID: 7, reader: iotest.ErrReader(randomError), want: randomError},
		{name: "empty source", userID: 7, reader: bytes.NewReader(nil), want: io.EOF},
		{name: "short source", userID: 7, reader: bytes.NewReader(make([]byte, 15)), want: io.ErrUnexpectedEOF},
		{name: "partial then failure", userID: 7, reader: io.MultiReader(bytes.NewReader(make([]byte, 7)), iotest.ErrReader(randomError)), want: randomError},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			uuid.SetRand(fixture.reader)
			t.Cleanup(func() { uuid.SetRand(nil) })
			key, err := GenerateUploadObjectKey(fixture.userID)
			if key != "" || err == nil {
				t.Fatal("生成失败应返回空键及错误")
			}
			if fixture.userID == 0 {
				if err.Error() != "生成上传对象键需要非零用户 ID" || errors.Is(err, randomError) {
					t.Fatal("零用户错误或检查顺序错误", err)
				}
			} else if !errors.Is(err, fixture.want) {
				t.Fatal("随机源失败原因未保留", err)
			}
		})
	}
}

func TestGenerateUploadObjectKeyIndependentAttempts(t *testing.T) {
	seen := make(map[string]bool)
	for attempt := 0; attempt < 128; attempt++ {
		key, err := GenerateUploadObjectKey(7)
		if err != nil {
			t.Fatal(err)
		}
		checkUploadObjectKey(t, 7, key)
		if seen[key] {
			t.Fatal("多次暂存尝试意外复用对象键")
		}
		seen[key] = true
	}
}

func TestGenerateUploadObjectKeyConcurrent(t *testing.T) {
	type generatedKey struct {
		key string
		err error
	}
	const workerCount, attemptsPerWorker = 32, 20
	results := make(chan generatedKey, workerCount*attemptsPerWorker)
	for worker := 0; worker < workerCount; worker++ {
		go func() {
			for attempt := 0; attempt < attemptsPerWorker; attempt++ {
				key, err := GenerateUploadObjectKey(7)
				results <- generatedKey{key: key, err: err}
			}
		}()
	}
	seen := make(map[string]bool)
	for resultIndex := 0; resultIndex < workerCount*attemptsPerWorker; resultIndex++ {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		checkUploadObjectKey(t, 7, result.key)
		if seen[result.key] {
			t.Fatal("并发暂存尝试意外复用对象键")
		}
		seen[result.key] = true
	}
}
