CREATE TABLE upload_parts (
 -- 1. 保存服务端核对的分片回执和核对时间。
 task_id BIGINT UNSIGNED NOT NULL COMMENT '上传任务ID',
 part_number INT UNSIGNED NOT NULL COMMENT '从1开始的分片编号',
 etag VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT '存储ETag原值',
 size BIGINT UNSIGNED NOT NULL COMMENT '分片字节数',
 checksum_algorithm VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NULL COMMENT '存储校验算法',
 checksum_value VARCHAR(255) CHARACTER SET ascii COLLATE ascii_bin NULL COMMENT '存储校验值原文',
 observed_at DATETIME(6) NOT NULL COMMENT '服务端核对时间UTC',
 -- 2. 同一任务的分片编号唯一，且必须有对应的上传任务。
 PRIMARY KEY (task_id,part_number),
 CONSTRAINT fk_upload_parts_task FOREIGN KEY (task_id) REFERENCES upload_tasks(id),
 -- 3. 限制合法编号，摘要算法和值同时存在或同时为空。
 CONSTRAINT ck_upload_parts_number CHECK (part_number BETWEEN 1 AND 10000),
 CONSTRAINT ck_upload_parts_checksum CHECK (
   (checksum_algorithm IS NULL AND checksum_value IS NULL)
   OR (checksum_algorithm IS NOT NULL AND checksum_value IS NOT NULL))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci COMMENT='上传分片核对回执';
