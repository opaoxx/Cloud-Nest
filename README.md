# CloudNest

CloudNest 是一套用于学习和自用的文件云盘。后端使用 Go、Gin、MySQL 和 MinIO，Web 使用 React、TypeScript 和 Vite。

- Web 支持登录、目录管理、16 MiB 内单文件上传、下载、图片与文本预览及回收站。
- 后端提供 S3 分片上传、同账号内容复用、条件内容替换、历史版本保留，以及快照和增量同步 API。
- Web 也提供独立的演示模式。桌面和移动客户端留在后续阶段。

## 本地运行

准备 Go 1.27.2、Node.js 24、pnpm 11.21.0、Python 3、Docker Compose 和 PowerShell。在仓库根目录启动依赖、初始化 MinIO 私有桶及数据库，再运行 API 服务。

```powershell
./scripts/Start-Infra.ps1
python scripts/verify_infra.py --service minio
$env:CLOUDNEST_ADMIN_PASSWORD = '<自行设置的本地登录密码>'
go run ./cmd/database -database cloudnest -seed
go run ./cmd/server
```

启动脚本会从 `scripts/.env.example` 创建本地 `scripts/.env`。示例配置用于回环地址上的开发环境。若修改 MySQL 或 MinIO 凭据，同时设置 Go 进程的 `CLOUDNEST_DB_PASSWORD`、`CLOUDNEST_S3_ACCESS_KEY` 和 `CLOUDNEST_S3_SECRET_KEY`。

另开一个终端启动 Web。

```powershell
pnpm --dir apps/web install --frozen-lockfile
$env:VITE_CLOUDNEST_DATA_MODE = 'api'
pnpm --dir apps/web dev
```

打开 [http://127.0.0.1:5173](http://127.0.0.1:5173)，使用业务账号 `root` 和初始化时设置的密码登录。首次初始化会应用数据库迁移；已有账号的密码沿用原值。API 默认监听 `127.0.0.1:18081`，可用 `GET /health/ready` 检查依赖。新终端直接运行 Web 开发命令即进入演示模式。

## 构建与接口

```powershell
go build ./cmd/...
pnpm --dir apps/web build
```

接口契约见 [OpenAPI](api/openapi.yaml)。数据库迁移由 `cmd/database` 执行，离线同步代次重置工具可通过 `go run ./cmd/restore-sync --help` 查看用法。
