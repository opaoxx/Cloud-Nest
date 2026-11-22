-- 1. 保存已被替换的逻辑版本和其不可变对象引用。
CREATE TABLE file_versions (
 id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT COMMENT '历史引用ID',
 user_id BIGINT UNSIGNED NOT NULL COMMENT '所属用户',
 entry_id BIGINT UNSIGNED NOT NULL COMMENT '被替换内容所属条目ID',
 revision BIGINT UNSIGNED NOT NULL COMMENT '替换前的条目版本',
 object_id BIGINT UNSIGNED NOT NULL COMMENT '旧内容对象ID',
 file_size BIGINT UNSIGNED NOT NULL COMMENT '保留引用的逻辑计费字节数',
 retained_until DATETIME(6) NOT NULL COMMENT '允许到期结算的UTC时间',
 created_at DATETIME(6) NOT NULL COMMENT '开始保留的UTC时间',
 -- 2. 约束版本唯一性、本账号归属和到期扫描顺序。
 PRIMARY KEY (id),
 UNIQUE KEY uk_file_versions_entry_revision (entry_id,revision),
 KEY idx_file_versions_owner_entry (user_id,entry_id),
 KEY idx_file_versions_owner_object (user_id,object_id),
 KEY idx_file_versions_expiry (retained_until,id),
 CONSTRAINT fk_file_versions_owner_entry FOREIGN KEY (user_id,entry_id) REFERENCES file_entries(user_id,id) ON DELETE RESTRICT,
 CONSTRAINT fk_file_versions_owner_object FOREIGN KEY (user_id,object_id) REFERENCES stored_objects(owner_user_id,id) ON DELETE RESTRICT,
 CONSTRAINT ck_file_versions_revision CHECK (revision>0),
 CONSTRAINT ck_file_versions_retention CHECK (retained_until>created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='被替换的文件内容引用';
