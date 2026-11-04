# CloudNest

学习导向的自用文件云盘，采用 Go 后端、React Web、Wails 桌面端和 Flutter 安卓端。

当前已完成本机开发环境、基础容器及首期八张业务表初始化。后端目前提供 /ping 和 /health/live，支持 HTTP/MySQL 配置读取校验、统一启动日志、MySQL 连接池和退出信号处理。启动时检查数据库连接与迁移记录，文件业务按技术方案分阶段实现。

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

启动 HTTP 前会在最多 5 秒内检查 MySQL 连接，失败记录原因并退出。因此运行服务前需要先启动本项目 MySQL；/ping 保留原有响应，不承担数据库持续健康检查。连接池及只读集成验证见 [第 004 步学习记录](docs/开发学习记录.md#第-004-步建立-mysql-连接池并检查连通性)。

连接通过后，还会在最多 5 秒内核对 schema_migrations 的版本、dirty 状态和迁移 SQL 摘要；当前只接受版本 1 的完整记录。检查失败则停止启动，迁移仍由独立命令执行。检查范围及自动清理隔离库的测试方法见 [第 005 步学习记录](docs/开发学习记录.md#第-005-步启动时检查数据库迁移记录)。

启动成功后，GET /health/live 无需登录，返回 {"status":"ok"}，表示 HTTP 服务能够响应。这个请求不检查数据库或存储的持续可用性；接口契约见 [OpenAPI](api/openapi.yaml)，路由和处理函数的调用过程见 [第 006 步学习记录](docs/开发学习记录.md#第-006-步提供存活检查并拆分-http-路由)。

按 Ctrl+C 后，服务停止接收新请求，最多等待在途请求 30 秒，随后关闭数据库连接池并退出；等待超时会强制关闭 HTTP 连接并报告失败。调用顺序和验证方法见 [第 007 步学习记录](docs/开发学习记录.md#第-007-步响应退出信号并关闭服务资源)。

Start-Infra.ps1 会先校验所有容器（包括已停止容器）的端口预留、主机端口绑定和子网，再按校验通过的官方源码构建本地 MinIO 并启动 CloudNest 容器。首次构建需要下载依赖。

## 本地连接信息

| 组件 | 本机地址 | 账号 | 密码 |
|---|---|---|---|
| MySQL 8.4.13 | 127.0.0.1:13309，数据库 cloudnest | root | root |
| Redis 7.4.12 | 127.0.0.1:16380，数据库 0 | root（ACL 用户） | root |
| MinIO S3 API | http://127.0.0.1:19000，私有桶 cloudnest | root | rootroot |
| MinIO 控制台 | [打开控制台](http://127.0.0.1:19001) | root | rootroot |
| Go API | [检查 /ping](http://127.0.0.1:18081/ping)、[存活检查](http://127.0.0.1:18081/health/live) | — | — |

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
