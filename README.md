# CloudNest

学习导向的自用文件云盘，采用 Go 后端、React Web、Wails 桌面端和 Flutter 安卓端。

当前已完成本机开发环境、基础容器及首期八张业务表初始化。后端目前提供 /ping、/health/live 和 /health/ready，支持配置及 CORS 白名单放行/预检、JSON 日志、依赖客户端、退出信号处理、请求 ID 和统一 404/500。启动时检查数据库连接、迁移记录及已有存储桶访问，文件业务按技术方案分阶段实现。

## 数据库迁移

开发库 cloudnest 已应用版本 1，包含八张业务表和 schema_migrations。业务账号为 root，开发密码为 root，密码以 bcrypt 存储，配额为 100 GiB，根目录已创建。

~~~powershell
go run ./cmd/database -database cloudnest -seed
~~~

重复执行会核对迁移摘要并复用已有账号与根目录。配置、隔离验证和异常处理参见 [迁移说明](migrations/README.md)。

## Web 页面 Demo

已提供可操作的前端假数据页面，直接访问 [CloudNest Demo](http://127.0.0.1:5173)。支持文件浏览、上传进度模拟、预览、搜索、收藏、移动与回收站，刷新后重置。

~~~powershell
pnpm --dir apps/web install
pnpm --dir apps/web dev
~~~

本 Demo 独立运行，不需要启动后端或容器。具体能力与演示边界见 [Web Demo 说明](apps/web/README.md)。

## 开发环境

在项目根目录打开 PowerShell：

~~~powershell
. ./scripts/Enter-Dev.ps1
./scripts/Start-Infra.ps1
python scripts/verify_infra.py
go run ./cmd/server
~~~

Enter-Dev.ps1 为当前 PowerShell 加载现有 Android SDK、ADB、Wails CLI 和后端地址；默认 SDK 为 F:\Android\Sdk，不修改系统级环境变量。

后端读取 CLOUDNEST_DB_ADDRESS、CLOUDNEST_DB_NAME、CLOUDNEST_DB_USER、CLOUDNEST_DB_PASSWORD，未设置时分别采用 127.0.0.1:13309、cloudnest、root、root。配置说明见 [第 003 步学习记录](docs/开发学习记录.md#第-003-步读取并校验-mysql-连接配置)。

CLOUDNEST_ALLOWED_ORIGINS 配置逗号分隔的前端来源，未设置时为 http://127.0.0.1:5173，例如 http://127.0.0.1:5173,http://localhost:5173。启动时校验、规范化并去重，无效配置停止启动；现已接入 CORS：允许来源附带明确跨域响应头，未知来源或非法预检返回统一 403；合法 OPTIONS 预检返回 204，无 Origin 请求沿用原流程。浏览器可读取 X-Request-ID，白名单包含来源不代表已经实现对应业务接口。具体规则与体验见 [第 016 步学习记录](docs/开发学习记录.md#第-016-步接入-cors-来源检查与预检处理)。

存储配置读取 CLOUDNEST_STORAGE_PROVIDER 及 CLOUDNEST_S3_ENDPOINT、PUBLIC_ENDPOINT、BUCKET、ACCESS_KEY、SECRET_KEY。默认 provider 为 local_minio，内部地址为 127.0.0.1:19000，公开地址继承内部地址，桶为 cloudnest，凭证为 root/rootroot。两个地址支持显式 HTTP/HTTPS；省略协议时按 HTTP 解析。配置规则见 [第 008 步学习记录](docs/开发学习记录.md#第-008-步读取并校验-minio-存储配置)。

后端现已在数据库检查后创建 MinIO 客户端，并在最多 5 秒内只读检查已有桶是否可访问；检查失败则停止启动，不自动创建桶。因此启动后端前需让 MySQL 与 MinIO 均处于可用状态。实现及只读测试方法见 [第 009 步学习记录](docs/开发学习记录.md#第-009-步创建-minio-客户端并检查已有桶)。

启动 HTTP 前会在最多 5 秒内检查 MySQL 连接，失败记录原因并退出。因此运行服务前需要先启动本项目 MySQL；/ping 保留原有响应，不承担数据库持续健康检查。连接池及只读集成验证见 [第 004 步学习记录](docs/开发学习记录.md#第-004-步建立-mysql-连接池并检查连通性)。

连接通过后，还会在最多 5 秒内核对 schema_migrations 的版本、dirty 状态和迁移 SQL 摘要；当前只接受版本 1 的完整记录。检查失败则停止启动，迁移仍由独立命令执行。检查范围及自动清理隔离库的测试方法见 [第 005 步学习记录](docs/开发学习记录.md#第-005-步启动时检查数据库迁移记录)。

启动成功后，GET /health/live 无需登录，返回 {"status":"ok"}，表示 HTTP 服务能够响应。这个请求不检查数据库或存储的持续可用性；接口契约见 [OpenAPI](api/openapi.yaml)，路由和处理函数的调用过程见 [第 006 步学习记录](docs/开发学习记录.md#第-006-步提供存活检查并拆分-http-路由)。

GET /health/ready 每次请求都复查 MySQL 连接和已有 MinIO 桶访问，共享最多 5 秒；全部可用返回 200 与 {"status":"ready"}，任一检查失败返回 503 与 {"status":"not_ready"}，响应不缓存也不返回底层错误。依赖故障时 live 仍可返回 200，恢复后下次 ready 重新检查。调用过程与验证见 [第 010 步学习记录](docs/开发学习记录.md#第-010-步提供依赖就绪检查接口)。

HTTP 响应现附带 X-Request-ID：单个标准 UUID v4 输入会保留并规范为小写，缺失、无效或重复输入则生成新值；同一个最终 ID 保存在请求上下文中。三个系统接口的 JSON 正文保持原样。体验及代码讲解见 [第 011 步学习记录](docs/开发学习记录.md#第-011-步增加请求-id-中间件)。

HTTP 处理完成后输出一条 JSON 日志，记录 request_id、method、path、status 和 duration_ms；可用响应头里的 ID 在终端查找对应请求。日志不记录查询参数、请求头或正文，异常恢复也不输出原始请求转储。字段、执行顺序和验证见 [第 012 步学习记录](docs/开发学习记录.md#第-012-步使用请求-id-记录-http-完成日志)。

未匹配到接口时，404 返回 error 与 request_id；错误码为 ROUTE_NOT_FOUND，details 明确为 null。正文、响应头和日志中的 ID 一致，便于查找对应请求。共享结构与体验方法见 [第 013 步学习记录](docs/开发学习记录.md#第-013-步统一未注册路由的-404-错误响应)。

普通 panic 被恢复且响应未提交时，返回相同错误结构的 500，错误码为 INTERNAL_ERROR，不回显原始异常。已提交的响应保持原样，不追加错误 JSON；完成日志以 ERROR 和 recovered_panic=true 标记异常，status 保留实际值。边界与测试方法见 [第 014 步学习记录](docs/开发学习记录.md#第-014-步统一可恢复异常的-500-响应)。

按 Ctrl+C 后，服务停止接收新请求，最多等待在途请求 30 秒，随后释放存储空闲连接、关闭数据库连接池并退出；等待超时会强制关闭 HTTP 连接并报告失败。信号与 HTTP 关闭的调用过程见 [第 007 步学习记录](docs/开发学习记录.md#第-007-步响应退出信号并关闭服务资源)。

Start-Infra.ps1 会先校验所有容器（包括已停止容器）的端口预留、主机端口绑定和子网，再按校验通过的官方源码构建本地 MinIO 并启动 CloudNest 容器。首次构建需要下载依赖。

## 本地连接信息

| 组件 | 本机地址 | 账号 | 密码 |
|---|---|---|---|
| MySQL 8.4.13 | 127.0.0.1:13309，数据库 cloudnest | root | root |
| Redis 7.4.12 | 127.0.0.1:16380，数据库 0 | root（ACL 用户） | root |
| MinIO S3 API | http://127.0.0.1:19000，私有桶 cloudnest | root | rootroot |
| MinIO 控制台 | [打开控制台](http://127.0.0.1:19001) | root | rootroot |
| Go API | [检查 /ping](http://127.0.0.1:18081/ping)、[存活检查](http://127.0.0.1:18081/health/live)、[就绪检查](http://127.0.0.1:18081/health/ready) | — | — |

按本项目约定，后续本地开发组件的账号、密码默认使用 root。MinIO 密码存在至少 8 位的硬性限制，因此使用 rootroot；此例外同时适用于控制台和 S3 secret key。

所有依赖端口只绑定 127.0.0.1。后端默认使用 18081，避开用户保留给其他项目的 8080–8090；18080 已被另一已停止容器预留，也不使用。

## 常用命令

~~~powershell
# 检查端口和子网；识别 CloudNest 自己已经运行的容器
python scripts/check_infra_ports.py

# 验证认证和真实 SQL、Redis、S3 分片读写，并创建私有 cloudnest 桶
python scripts/verify_infra.py

# 查看或停止本项目依赖，保留数据卷
docker compose -f scripts/docker-compose.yml ps
docker compose -f scripts/docker-compose.yml stop

# Go 代码检查
go test ./...
go vet ./...
~~~

私有桶由 verify_infra.py 在不存在时创建；验证临时对象会自动清理。三个组件分别使用 cloudnest_mysql_data、cloudnest_redis_data、cloudnest_minio_data 命名卷，独立网络为 cloudnest-dev（10.253.81.0/24）。

本地配置来自 scripts/.env，示例为 scripts/.env.example。修改已初始化数据库的密码需要同步修改数据库账号，不能只修改环境变量。

## 项目文档

- [教学开发约定](AGENTS.md)
- [开发学习记录](docs/开发学习记录.md)
- [当前 HTTP 接口契约](api/openapi.yaml)

- [技术方案书](<云盘 SaaS 项目技术方案书（混合架构版）.md>)
- [数据库设计方案书（已批准）](docs/数据库设计方案书.md)
- [后端设计方案书（已批准）](docs/后端设计方案书.md)
- [环境检查、版本与验证记录](docs/development_environment.md)
- [Bug 修复记录](bugfix_log.md)
