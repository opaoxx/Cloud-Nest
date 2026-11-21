// Command database applies the reviewed initial schema and optionally seeds a local account.
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/opaoxx/cloud-nest/migrations"
	"golang.org/x/crypto/bcrypt"
)

func main() {
	databaseName := flag.String("database", "cloudnest", "Existing target database")
	seed := flag.Bool("seed", false, "Create the local root business account and its root directory")
	flag.Parse()
	if err := run(*databaseName, *seed); err != nil {
		log.Fatal(err)
	}
}

func envOrDefault(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

func run(databaseName string, seed bool) error {
	config := mysql.NewConfig()
	config.User = envOrDefault("CLOUDNEST_DB_USER", "root")
	config.Passwd = envOrDefault("CLOUDNEST_DB_PASSWORD", "root")
	config.Net = "tcp"
	config.Addr = envOrDefault("CLOUDNEST_DB_ADDRESS", "127.0.0.1:13309")
	config.DBName = databaseName
	config.MultiStatements = true
	config.ParseTime = true
	config.Timeout = 10 * time.Second
	config.ReadTimeout = 60 * time.Second
	config.WriteTimeout = 60 * time.Second
	config.Params = map[string]string{"charset": "utf8mb4", "time_zone": "'+00:00'"}
	connectionPool, err := sql.Open("mysql", config.FormatDSN())
	if err != nil {
		return err
	}
	defer connectionPool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	connection, err := connectionPool.Conn(ctx)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer connection.Close()

	lockName := fmt.Sprintf("cloudnest-schema-%x", sha256.Sum256([]byte(databaseName)))[:63]
	var acquired int
	if err := connection.QueryRowContext(ctx, "SELECT GET_LOCK(?, 10)", lockName).Scan(&acquired); err != nil {
		return err
	}
	if acquired != 1 {
		return errors.New("another database migration is running")
	}
	defer func() {
		releaseContext, releaseCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer releaseCancel()
		_, _ = connection.ExecContext(releaseContext, "SELECT RELEASE_LOCK(?)", lockName)
	}()

	expectedMigrations := migrations.Ordered()
	if err := migrate(ctx, connection, expectedMigrations); err != nil {
		return err
	}
	if seed {
		if err := seedAccount(ctx, connection, envOrDefault("CLOUDNEST_ADMIN_PASSWORD", "root")); err != nil {
			return err
		}
	}
	fmt.Printf("Database %s is at schema version %d.\n", databaseName, expectedMigrations[0].Version)
	return nil
}

func migrate(ctx context.Context, connection *sql.Conn, expectedMigrations []migrations.Migration) error {
	// 1. 在写入或执行 DDL 前，核对已应用记录是否与清单开头连续匹配。
	appliedCount, err := readAppliedMigrationCount(ctx, connection, expectedMigrations)
	if err != nil {
		return err
	}

	// 2. 当前命令只执行首期迁移。历史已覆盖该版本时直接返回。
	initialMigration := expectedMigrations[0]
	if appliedCount > 0 {
		fmt.Printf("Migration %d already applied; checksum verified.\n", initialMigration.Version)
		return nil
	}

	// 3. 业务库为空时才准备迁移表，按 dirty → 执行 SQL → 完成的顺序初始化。
	var otherTables int
	if err := connection.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name <> 'schema_migrations'").Scan(&otherTables); err != nil {
		return err
	}
	if otherTables != 0 {
		return errors.New("initial migration requires an empty business schema; existing tables need explicit reconciliation")
	}
	if _, err := connection.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
 version BIGINT UNSIGNED NOT NULL PRIMARY KEY COMMENT '迁移版本',
 name VARCHAR(255) NOT NULL COMMENT '迁移文件',
 checksum CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT '迁移内容SHA256',
 dirty BOOLEAN NOT NULL COMMENT '部分执行标记',
 started_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) COMMENT '开始时间UTC',
 applied_at DATETIME(6) NULL COMMENT '成功时间UTC'
 ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='数据库结构迁移记录'`); err != nil {
		return err
	}
	if _, err := connection.ExecContext(ctx, "INSERT INTO schema_migrations(version,name,checksum,dirty) VALUES(?,?,?,true)", initialMigration.Version, initialMigration.Name, initialMigration.Checksum()); err != nil {
		return err
	}
	if _, err := connection.ExecContext(ctx, initialMigration.SQL); err != nil {
		return fmt.Errorf("migration failed and is marked dirty: %w", err)
	}
	if _, err := connection.ExecContext(ctx, "UPDATE schema_migrations SET dirty=false, applied_at=UTC_TIMESTAMP(6) WHERE version=?", initialMigration.Version); err != nil {
		return err
	}
	fmt.Printf("Migration %d applied successfully.\n", initialMigration.Version)
	return nil
}

// readAppliedMigrationCount 返回与清单开头连续匹配的已应用版本数。调用方持有迁移命名锁。
func readAppliedMigrationCount(ctx context.Context, connection *sql.Conn, expectedMigrations []migrations.Migration) (int, error) {
	// 1. 按版本读取，额外读取一条即可发现超出清单的记录。
	rows, err := connection.QueryContext(ctx,
		"SELECT version, checksum, dirty FROM schema_migrations ORDER BY version LIMIT ?", len(expectedMigrations)+1)
	if err != nil {
		var mysqlError *mysql.MySQLError
		if errors.As(err, &mysqlError) && mysqlError.Number == 1146 {
			return 0, nil
		}
		return 0, fmt.Errorf("read migration history: %w", err)
	}
	defer rows.Close()

	// 2. 从清单首项逐条核对，发现跳号、未知版本、dirty 或摘要失配时停止。
	appliedCount := 0
	for rows.Next() {
		var version uint64
		var checksum string
		var dirty bool
		if err := rows.Scan(&version, &checksum, &dirty); err != nil {
			return 0, fmt.Errorf("read migration record: %w", err)
		}
		if appliedCount == len(expectedMigrations) {
			return 0, fmt.Errorf("migration %d is outside the supported migration list", version)
		}
		expectedMigration := expectedMigrations[appliedCount]
		if version != expectedMigration.Version {
			return 0, fmt.Errorf("migration %d found, expected migration %d", version, expectedMigration.Version)
		}
		if dirty {
			return 0, fmt.Errorf("migration %d is dirty; inspect the partial DDL and restore before retrying", version)
		}
		if checksum != expectedMigration.Checksum() {
			return 0, fmt.Errorf("migration %d checksum differs; add a new migration instead of editing an applied file", version)
		}
		appliedCount++
	}

	// 3. 确认读取完整后返回条数，清单末尾尚未应用的版本允许留空。
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("read migration history: %w", err)
	}
	return appliedCount, nil
}

func seedAccount(ctx context.Context, connection *sql.Conn, password string) error {
	transaction, err := connection.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer transaction.Rollback()
	var userID uint64
	err = transaction.QueryRowContext(ctx, "SELECT id FROM users WHERE username='root' FOR UPDATE").Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		passwordHash, hashErr := bcrypt.GenerateFromPassword([]byte(password), 12)
		if hashErr != nil {
			return hashErr
		}
		result, insertErr := transaction.ExecContext(ctx, "INSERT INTO users(username,display_name,password_hash) VALUES('root','root',?)", passwordHash)
		if insertErr != nil {
			return insertErr
		}
		insertedID, idErr := result.LastInsertId()
		if idErr != nil {
			return idErr
		}
		userID = uint64(insertedID)
	} else if err != nil {
		return err
	}

	var rootID uint64
	err = transaction.QueryRowContext(ctx, "SELECT id FROM file_entries WHERE root_owner_id=? FOR UPDATE", userID).Scan(&rootID)
	if errors.Is(err, sql.ErrNoRows) {
		result, insertErr := transaction.ExecContext(ctx, "INSERT INTO file_entries(user_id,parent_id,name,name_key,entry_type) VALUES(?,NULL,'',X'','DIR')", userID)
		if insertErr != nil {
			return insertErr
		}
		insertedID, idErr := result.LastInsertId()
		if idErr != nil {
			return idErr
		}
		rootID = uint64(insertedID)
	} else if err != nil {
		return err
	}
	if err := transaction.Commit(); err != nil {
		return err
	}
	fmt.Printf("Business account root ready (user_id=%d, root_entry_id=%d).\n", userID, rootID)
	return nil
}
