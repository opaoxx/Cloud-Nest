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

	if err := migrate(ctx, connection); err != nil {
		return err
	}
	if seed {
		if err := seedAccount(ctx, connection, envOrDefault("CLOUDNEST_ADMIN_PASSWORD", "root")); err != nil {
			return err
		}
	}
	fmt.Printf("Database %s is at schema version 1.\n", databaseName)
	return nil
}

func migrate(ctx context.Context, connection *sql.Conn) error {
	_, err := connection.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
 version BIGINT UNSIGNED NOT NULL PRIMARY KEY COMMENT '迁移版本',
 name VARCHAR(255) NOT NULL COMMENT '迁移文件',
 checksum CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT '迁移内容SHA256',
 dirty BOOLEAN NOT NULL COMMENT '部分执行标记',
 started_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) COMMENT '开始时间UTC',
 applied_at DATETIME(6) NULL COMMENT '成功时间UTC'
 ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='数据库结构迁移记录'`)
	if err != nil {
		return err
	}
	checksum := fmt.Sprintf("%x", sha256.Sum256([]byte(migrations.InitialSQL)))
	var existingChecksum string
	var dirty bool
	err = connection.QueryRowContext(ctx, "SELECT checksum, dirty FROM schema_migrations WHERE version=1").Scan(&existingChecksum, &dirty)
	if err == nil {
		if dirty {
			return errors.New("migration 1 is dirty; inspect the partial DDL and restore before retrying")
		}
		if existingChecksum != checksum {
			return errors.New("migration 1 checksum differs; add a new migration instead of editing an applied file")
		}
		fmt.Println("Migration 1 already applied; checksum verified.")
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var otherTables int
	if err := connection.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name <> 'schema_migrations'").Scan(&otherTables); err != nil {
		return err
	}
	if otherTables != 0 {
		return errors.New("initial migration requires an empty business schema; existing tables need explicit reconciliation")
	}
	if _, err := connection.ExecContext(ctx, "INSERT INTO schema_migrations(version,name,checksum,dirty) VALUES(1,?, ?,true)", "000001_initial.up.sql", checksum); err != nil {
		return err
	}
	if _, err := connection.ExecContext(ctx, migrations.InitialSQL); err != nil {
		return fmt.Errorf("migration failed and is marked dirty: %w", err)
	}
	if _, err := connection.ExecContext(ctx, "UPDATE schema_migrations SET dirty=false, applied_at=UTC_TIMESTAMP(6) WHERE version=1"); err != nil {
		return err
	}
	fmt.Println("Migration 1 applied successfully.")
	return nil
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
