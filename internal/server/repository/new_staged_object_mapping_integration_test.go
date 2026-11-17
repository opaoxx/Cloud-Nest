package repository

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/internal/server/domain"
	"github.com/opaoxx/cloud-nest/internal/server/repository/models"
)

func TestNewStagedObjectMappingIntegration(t *testing.T) {
	// 1. 复用独占库和正式表，准备可信测试用户。
	pool := userProfileDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash) VALUES(7,'staged_owner','暂存用户','fixture_hash'),(8,'staged_other','另一用户','fixture_hash')"); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	checkInsert := func(t *testing.T, userID uint64, input domain.CreateStagedObjectInput) {
		t.Helper()
		// 2. 用新映射插入六列，按归属及位置唯一键取得数据库标识。
		record := models.NewStagedObject{
			OwnerUserID: userID, Provider: input.Provider, Bucket: input.Bucket, ObjectKey: input.ObjectKey,
			FileSize: input.SizeBytes, Status: string(domain.StoredObjectStatusStaging),
		}
		original := record
		result := store.database.WithContext(ctx).Create(&record)
		if result.Error != nil || result.RowsAffected != 1 || record != original {
			t.Fatal("真实暂存对象映射插入失败或改写了来源", result.Error)
		}
		var objectID uint64
		if err := pool.QueryRowContext(ctx, "SELECT id FROM stored_objects WHERE owner_user_id=? AND provider=? AND bucket=? AND object_key=?", userID, input.Provider, input.Bucket, input.ObjectKey).Scan(&objectID); err != nil || objectID == 0 {
			t.Fatal("按归属及位置回读自增对象标识失败", err)
		}
		actual, err := store.FindUserStoredObject(ctx, userID, objectID)
		if err != nil {
			t.Fatal(err)
		}
		// 3. 核对 STAGING、八项 NULL、自动时间和完整声明大小。
		want := domain.StoredObject{
			ID: objectID, OwnerUserID: userID, Provider: input.Provider, Bucket: input.Bucket, ObjectKey: input.ObjectKey,
			FileSize: input.SizeBytes, Status: domain.StoredObjectStatusStaging,
			CreatedAt: actual.CreatedAt, UpdatedAt: actual.UpdatedAt,
		}
		if !reflect.DeepEqual(actual, &want) || actual.CreatedAt.IsZero() || actual.CreatedAt != actual.UpdatedAt {
			t.Fatal("暂存对象数据库默认值或写入字段不符")
		}
	}
	for _, fixture := range []struct {
		name  string
		input domain.CreateStagedObjectInput
	}{
		{"zero-byte object", domain.CreateStagedObjectInput{Provider: "local_minio", Bucket: "cloudnest", ObjectKey: "staged/empty"}},
		{"ordinary object", domain.CreateStagedObjectInput{Provider: "local_minio", Bucket: "cloudnest", ObjectKey: "staged/ordinary", SizeBytes: 2048}},
		{"maximum text lengths", domain.CreateStagedObjectInput{Provider: strings.Repeat("p", 32), Bucket: strings.Repeat("b", 63), ObjectKey: strings.Repeat("k", 255), SizeBytes: 1}},
	} {
		t.Run(fixture.name, func(t *testing.T) { checkInsert(t, 7, fixture.input) })
	}
	t.Run("location uniqueness and owner foreign key", func(t *testing.T) {
		for _, fixture := range []struct {
			userID uint64
			key    string
			number uint16
		}{
			{8, "staged/ordinary", 1062},
			{999, "staged/missing-owner", 1452},
		} {
			record := models.NewStagedObject{OwnerUserID: fixture.userID, Provider: "local_minio", Bucket: "cloudnest", ObjectKey: fixture.key, Status: "STAGING"}
			err := store.database.WithContext(ctx).Create(&record).Error
			var databaseError *mysql.MySQLError
			if !errors.As(err, &databaseError) || databaseError.Number != fixture.number {
				t.Fatal("暂存对象映射未保留真实位置唯一键或归属外键错误", err)
			}
		}
		var count int
		if err := pool.QueryRowContext(ctx, "SELECT COUNT(*) FROM stored_objects").Scan(&count); err != nil || count != 3 {
			t.Fatal("失败的映射插入改变了对象记录数", err)
		}
	})
	t.Run("maximum unsigned owner and size", func(t *testing.T) {
		maximum := ^uint64(0)
		if _, err := pool.ExecContext(ctx, "INSERT INTO users(id,username,display_name,password_hash) VALUES(?,'staged_max','完整整数用户','fixture_hash')", maximum); err != nil {
			t.Fatal(err)
		}
		checkInsert(t, maximum, domain.CreateStagedObjectInput{Provider: "local_minio", Bucket: "cloudnest", ObjectKey: "staged/maximum", SizeBytes: maximum})
	})
}
