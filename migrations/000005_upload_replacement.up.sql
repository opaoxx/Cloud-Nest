ALTER TABLE upload_tasks
 -- 1. 增加替换绑定和结果版本，既有普通上传的四列均为 NULL。
 ADD COLUMN replacement_entry_id BIGINT UNSIGNED NULL COMMENT '内容替换目标条目',
 ADD COLUMN replacement_expected_revision BIGINT UNSIGNED NULL COMMENT '创建时绑定的条目版本',
 ADD COLUMN replacement_published_revision BIGINT UNSIGNED NULL COMMENT '替换提交后的条目版本',
 ADD COLUMN replacement_conflicting_revision BIGINT UNSIGNED NULL COMMENT '发布冲突时锁读的条目版本',
 -- 2. 约束替换目标属于同一账号，绑定必须成对且附带整文件摘要。
 ADD KEY idx_uploads_replacement (user_id,replacement_entry_id),
 ADD CONSTRAINT fk_uploads_replacement FOREIGN KEY (user_id,replacement_entry_id) REFERENCES file_entries(user_id,id),
 ADD CONSTRAINT ck_uploads_replacement_binding CHECK (
   (replacement_entry_id IS NULL AND replacement_expected_revision IS NULL)
   OR (replacement_entry_id IS NOT NULL AND replacement_entry_id>0
     AND replacement_expected_revision IS NOT NULL AND replacement_expected_revision>0 AND expected_sha256 IS NOT NULL)
 ),
 -- 3. 校验已发布或冲突的版本快照。用 DECIMAL 比较，避免无符号版本上限加一溢出。
 ADD CONSTRAINT ck_uploads_replacement_result CHECK (
   CASE
     WHEN replacement_entry_id IS NULL THEN
       replacement_published_revision IS NULL AND replacement_conflicting_revision IS NULL
     WHEN status='COMPLETED' THEN
       result_entry_id <=> replacement_entry_id
       AND replacement_published_revision IS NOT NULL
       AND replacement_published_revision=CAST(replacement_expected_revision AS DECIMAL(20,0))+1
       AND replacement_conflicting_revision IS NULL
     WHEN status='FAILED' AND last_error_code <=> 'REVISION_CONFLICT' THEN
       result_entry_id IS NULL AND replacement_published_revision IS NULL
       AND replacement_conflicting_revision IS NOT NULL AND replacement_conflicting_revision>0
       AND replacement_conflicting_revision<>replacement_expected_revision
     ELSE
       result_entry_id IS NULL AND replacement_published_revision IS NULL AND replacement_conflicting_revision IS NULL
   END = TRUE
 );
