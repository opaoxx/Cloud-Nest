-- 1. 创建按账号和序号读取的变更事件表。
CREATE TABLE change_events (
 user_id BIGINT UNSIGNED NOT NULL COMMENT '所属用户',
 `sequence` BIGINT UNSIGNED NOT NULL COMMENT '账号内已提交的变更序号',
 entry_id BIGINT UNSIGNED NOT NULL COMMENT '历史事件中的逻辑条目ID',
 revision BIGINT UNSIGNED NOT NULL COMMENT '变更后的条目版本',
 event_type VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT '变更类型',
 payload JSON NOT NULL COMMENT '变更提交时的事件载荷',
 created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) COMMENT '创建时间UTC',
 PRIMARY KEY (user_id,`sequence`),
 CONSTRAINT fk_change_events_user FOREIGN KEY (user_id) REFERENCES users(id),
 CONSTRAINT ck_change_events_sequence CHECK (`sequence`>0),
 CONSTRAINT ck_change_events_entry CHECK (entry_id>0),
 CONSTRAINT ck_change_events_revision CHECK (revision>0),
 CONSTRAINT ck_change_events_type CHECK (CHAR_LENGTH(event_type)>0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='账号变更日志';

-- 2. 创建绑定同账号认证设备的同步状态表。
CREATE TABLE sync_devices (
 device_id BIGINT UNSIGNED NOT NULL COMMENT '认证设备ID',
 user_id BIGINT UNSIGNED NOT NULL COMMENT '所属用户',
 ack_sequence BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT '设备确认的连续变更位置',
 status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'RESET_REQUIRED' COMMENT '同步基线状态',
 cursor_generation BINARY(16) NOT NULL COMMENT '服务端生成的设备同步代次',
 last_seen_at DATETIME(6) NOT NULL COMMENT '最近成功同步时间UTC',
 created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) COMMENT '创建时间UTC',
 updated_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6) COMMENT '更新时间UTC',
 PRIMARY KEY (device_id),
 KEY idx_sync_devices_owner_device (user_id,device_id),
 CONSTRAINT fk_sync_devices_owner_device FOREIGN KEY (user_id,device_id) REFERENCES user_devices(user_id,id),
 CONSTRAINT ck_sync_devices_status CHECK (CAST(status AS BINARY) IN ('ACTIVE','RESET_REQUIRED'))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='设备同步位置';
