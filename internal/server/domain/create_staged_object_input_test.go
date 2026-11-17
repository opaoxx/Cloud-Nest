package domain_test

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestCreateStagedObjectInputShape(t *testing.T) {
	// 1. 对照正式表列类型，确认四项输入及列长度边界。
	_, table, found := strings.Cut(migrations.InitialSQL, "CREATE TABLE stored_objects (\n")
	if !found {
		t.Fatal("正式迁移缺少存储对象表")
	}
	table, _, _ = strings.Cut(table, ") ENGINE=")
	inputType := reflect.TypeFor[domain.CreateStagedObjectInput]()
	if inputType.NumField() != 4 {
		t.Fatal("暂存对象创建输入字段数量错误")
	}
	for _, fixture := range []struct {
		fieldName string
		column    string
		sqlType   string
		fieldType reflect.Type
		maxLength int
	}{
		{"Provider", "provider", "VARCHAR(32)", reflect.TypeFor[string](), 32},
		{"Bucket", "bucket", "VARCHAR(63)", reflect.TypeFor[string](), 63},
		{"ObjectKey", "object_key", "VARCHAR(255)", reflect.TypeFor[string](), 255},
		{"SizeBytes", "file_size", "BIGINT UNSIGNED", reflect.TypeFor[uint64](), 0},
	} {
		t.Run(fixture.fieldName, func(t *testing.T) {
			field, exists := inputType.FieldByName(fixture.fieldName)
			pattern := `(?m)^ ` + fixture.column + ` ` + regexp.QuoteMeta(fixture.sqlType) + `([^\n]*)`
			column := regexp.MustCompile(pattern).FindStringSubmatch(table)
			if !exists || field.Type != fixture.fieldType || field.Tag != "" || len(column) != 2 || !strings.Contains(column[1], "NOT NULL") {
				t.Fatal("暂存对象创建输入的字段、列类型或空值约定错误")
			}
			if fixture.maxLength == 0 {
				return
			}
			// 2. 用迁移对应的列宽核对恰好达到与超过边界的输入。
			if !strings.Contains(column[1], "CHARACTER SET ascii COLLATE ascii_bin") {
				t.Fatal("暂存对象位置列的 ASCII 约定发生变化")
			}
			input := domain.CreateStagedObjectInput{Provider: "local_minio", Bucket: "cloudnest", ObjectKey: "users/7/objects/test"}
			value := reflect.ValueOf(&input).Elem().FieldByName(fixture.fieldName)
			value.SetString(strings.Repeat("a", fixture.maxLength))
			if !input.IsValid() {
				t.Fatal("存储位置恰好达到列宽时被拒绝")
			}
			value.SetString(strings.Repeat("a", fixture.maxLength+1))
			if input.IsValid() {
				t.Fatal("存储位置超过列宽时被接受")
			}
		})
	}
}

func TestCreateStagedObjectInputIsValid(t *testing.T) {
	// 1. 为三项位置文本逐项准备必填、长度及编码边界。
	for _, fieldName := range []string{"Provider", "Bucket", "ObjectKey"} {
		for _, fixture := range []struct {
			name  string
			text  string
			valid bool
		}{
			{"empty", "", false},
			{"one ASCII byte", "a", true},
			{"ASCII original spelling", "A_1.-/value", true},
			{"Chinese", "对象", false},
			{"emoji", "🌱", false},
			{"full-width text", "Ａ", false},
			{"invalid UTF-8", string([]byte{0xff}), false},
			{"truncated UTF-8", string([]byte{0xe5, 0xad}), false},
		} {
			t.Run(fieldName+"/"+fixture.name, func(t *testing.T) {
				input := domain.CreateStagedObjectInput{Provider: "local_minio", Bucket: "cloudnest", ObjectKey: "users/7/objects/test", SizeBytes: 2048}
				reflect.ValueOf(&input).Elem().FieldByName(fieldName).SetString(fixture.text)
				original := input
				// 2. 重复检查只读取输入，保留原始大小写、位置及大小。
				for range 3 {
					if input.IsValid() != fixture.valid || input != original {
						t.Fatal("暂存对象输入检查结果或原值保护错误")
					}
				}
			})
		}
	}
	if (domain.CreateStagedObjectInput{}).IsValid() {
		t.Fatal("暂存对象创建输入的零值被接受")
	}
}

func TestCreateStagedObjectInputSizePreservation(t *testing.T) {
	// 1. 存储输入保留零字节、上传上限附近和完整 uint64 值。
	for _, size := range []uint64{0, 1, 16777215, 16777216, 16777217, 1<<53 - 1, 1 << 63, ^uint64(0)} {
		t.Run(fmt.Sprintf("size %d", size), func(t *testing.T) {
			input := domain.CreateStagedObjectInput{
				Provider: "local_minio", Bucket: "cloudnest", ObjectKey: "users/7/objects/test", SizeBytes: size,
			}
			original := input
			if !input.IsValid() || input != original {
				t.Fatal("暂存对象输入截断或改写声明大小")
			}
			// 2. 对齐对象字段，上传声明继续独立执行首期大小上限。
			object := domain.StoredObject{Provider: input.Provider, Bucket: input.Bucket, ObjectKey: input.ObjectKey, FileSize: input.SizeBytes}
			if object.FileSize != size || object.Provider != input.Provider || object.Bucket != input.Bucket || object.ObjectKey != input.ObjectKey {
				t.Fatal("暂存对象创建字段衔接错误")
			}
			upload := domain.CreateUploadInput{ParentID: 13, Name: "file.bin", SizeBytes: size}
			if upload.IsValid() != (size <= 16777216) {
				t.Fatal("上传声明的首期大小上限发生变化")
			}
		})
	}
}
