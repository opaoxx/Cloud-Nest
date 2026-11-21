package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/migrations"
)

func TestMigrateInOrder(t *testing.T) {
	expectedMigrations := []migrations.Migration{
		{Version: 1, Name: "fixture_1.sql", SQL: "CREATE TABLE migration_fixture (id INT PRIMARY KEY)"},
		{Version: 2, Name: "fixture_2.sql", SQL: "ALTER TABLE migration_fixture ADD COLUMN payload INT NOT NULL"},
		{Version: 3, Name: "fixture_3.sql", SQL: "INSERT INTO migration_fixture (id, payload) VALUES (1, 42)"},
	}

	for _, testCase := range []struct {
		name         string
		appliedCount int
	}{
		{name: "empty schema"},
		{name: "resume after version 1", appliedCount: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			connection := openEmptyMigrationFixture(t)
			if testCase.appliedCount > 0 {
				if err := migrate(t.Context(), connection, expectedMigrations[:testCase.appliedCount]); err != nil {
					t.Fatal(err)
				}
			}
			if err := migrate(t.Context(), connection, expectedMigrations); err != nil {
				t.Fatal(err)
			}
			if err := migrate(t.Context(), connection, expectedMigrations); err != nil {
				t.Fatalf("repeat migration: %v", err)
			}
			assertFixtureMigrationRecords(t, connection, expectedMigrations, len(expectedMigrations))
			var payload int
			if err := connection.QueryRowContext(t.Context(), "SELECT payload FROM migration_fixture WHERE id=1").Scan(&payload); err != nil || payload != 42 {
				t.Fatalf("dependent migration wrote payload %d, error %v", payload, err)
			}
		})
	}

	t.Run("stop on partial DDL and reject retry", func(t *testing.T) {
		connection := openEmptyMigrationFixture(t)
		failingMigrations := append([]migrations.Migration(nil), expectedMigrations...)
		failingMigrations[1].SQL += "; INSERT INTO missing_fixture (id) VALUES (1)"
		err := migrate(t.Context(), connection, failingMigrations)
		var mysqlError *mysql.MySQLError
		if !errors.As(err, &mysqlError) || mysqlError.Number != 1146 || !strings.Contains(err.Error(), "migration 2 failed and is marked dirty") {
			t.Fatalf("migration did not stop at the second version: %v", err)
		}
		assertFixtureMigrationRecords(t, connection, failingMigrations[:2], 1)

		if err := migrate(t.Context(), connection, failingMigrations); err == nil || !strings.Contains(err.Error(), "migration 2 is dirty") {
			t.Fatalf("repeated migration did not reject the dirty record: %v", err)
		}
		assertFixtureMigrationRecords(t, connection, failingMigrations[:2], 1)
		var rowCount int
		if err := connection.QueryRowContext(t.Context(), "SELECT COUNT(payload) FROM migration_fixture").Scan(&rowCount); err != nil || rowCount != 0 {
			t.Fatalf("expected the partial column to remain and the final insert to be skipped, rows=%d error=%v", rowCount, err)
		}
	})
}

func openEmptyMigrationFixture(t *testing.T) *sql.Conn {
	t.Helper()
	database := verificationDB(t)
	connection, err := database.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })

	// 单独创建空库，在本连接执行迁移，避免写入现有测试基线。
	fixtureName := fmt.Sprintf("cloudnest_migrate_verify_fixture_%d", time.Now().UnixNano())
	if _, err := connection.ExecContext(t.Context(), "CREATE DATABASE "+fixtureName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := connection.ExecContext(cleanupContext, "DROP DATABASE "+fixtureName); err != nil {
			t.Errorf("remove migration fixture: %v", err)
		}
	})
	if _, err := connection.ExecContext(t.Context(), "USE "+fixtureName); err != nil {
		t.Fatal(err)
	}
	return connection
}

func assertFixtureMigrationRecords(t *testing.T, connection *sql.Conn, expectedMigrations []migrations.Migration, completedCount int) {
	t.Helper()
	rows, err := connection.QueryContext(t.Context(), "SELECT version,name,checksum,dirty,started_at,applied_at FROM schema_migrations ORDER BY version")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	for index, migration := range expectedMigrations {
		if !rows.Next() {
			t.Fatalf("missing record for version %d (%v)", migration.Version, rows.Err())
		}
		var version uint64
		var name, checksum string
		var dirty bool
		var startedAt time.Time
		var appliedAt sql.NullTime
		if err := rows.Scan(&version, &name, &checksum, &dirty, &startedAt, &appliedAt); err != nil {
			t.Fatal(err)
		}
		completed := index < completedCount
		if version != migration.Version || name != migration.Name || checksum != migration.Checksum() ||
			startedAt.IsZero() || dirty == completed || appliedAt.Valid != completed {
			t.Fatalf("unexpected record for version %d: version=%d name=%s checksum=%s dirty=%t started=%v applied=%v",
				migration.Version, version, name, checksum, dirty, startedAt, appliedAt)
		}
	}
	if rows.Next() {
		t.Fatal("unexpected extra migration record")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
