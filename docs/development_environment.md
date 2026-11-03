# CloudNest 开发环境检查记录

检查日期：2026-11-03。范围为开发工具、当前 Go 工程可构建性，以及本地基础容器的可用性。以下结果不代表桌面/移动业务或数据库集成已经实现。

## 工具与框架

| 项目 | 检查结果 |
|---|---|
| Windows | Windows 11，amd64，32 GB 内存 |
| Git | 2.53.0.windows.2 |
| Go / gofmt | go1.27.2 windows/amd64，与当前 go.mod 一致 |
| 当前后端 | Gin v1.12.0；go test ./...、go vet ./... 通过；目前没有自动测试用例 |
| Node / npm / pnpm | Node v24.14.1；npm 11.11.0；pnpm 11.21.0 |
| Docker | Docker Desktop 4.88.1；Engine 29.7.2；Compose v5.4.0；desktop-linux |
| Wails | 已安装 v3.0.0，位于 Go workspace 的 bin 目录；wails3 doctor 通过 |
| WebView2 | 156.0.4314.58，已由 Wails doctor 检测 |
| Flutter | E:\flutter，master 3.50.0-1.0.pre-232，提交 66b3ad9017 |
| Dart | 3.15.0-72.0.dev，与当前 Flutter 配套 |
| Android SDK | F:\Android\Sdk；doctor 检测 SDK 36.0.0、platform android-37.0 |
| Android build-tools / ADB | build-tools 36.0.0；ADB 37.0.1-15733141 |
| Android Java | Flutter 使用 F:\Android\Android Studio\jbr\bin\java，JDK 25.0.4；系统命令 java 为 JDK 21.0.10 |
| Android 许可证 | 已接受，flutter doctor -v 全部通过 |
| Visual Studio | Community 2022 17.14.0，已安装 C++ 工具及 Windows SDK，Flutter 检查通过 |
| Python | C:\Python314\python.exe，用于不依赖第三方包的基础设施检查脚本 |

当前执行会话最初没有拿到 Android SDK 路径；使用用户已安装的 F:\Android\Sdk 加载会话环境后，flutter doctor 全部通过。项目 Enter-Dev.ps1 提供相同的可重复加载方式，未重装 SDK，也未修改系统级环境变量。

Wails doctor 中的 Android NDK、NSIS、签名和打包工具为当前 Windows 开发之外的可选能力；安装包打包、签名和真机应用构建仍按阶段 0/3 单独验证。

当前 Flutter 为 master 预发布版本。此次保留已有全局 SDK；开始正式移动端开发时应为项目单独确定并锁定合适的稳定版本，避免影响其他项目。

React、Ant Design、Zustand、Flutter Riverpod 等属于项目依赖，当前尚无对应客户端应用目录。GORM、MySQL/Redis/S3 Go 客户端也需随所属业务功能引入并锁定版本；不把全局安装框架当作项目初始化完成。

## 端口与网络隔离

| 用途 | 本机端口 | 容器内端口 |
|---|---|---|
| MySQL | 13309 | 3306 |
| Redis | 16380 | 6379 |
| MinIO S3 API | 19000 | 9000 |
| MinIO 控制台 | 19001 | 9001 |
| 后端默认地址 | 18081 | 当前在宿主机运行 |

所有容器映射绑定 127.0.0.1。检查包含 Windows 实际监听、独占 socket 绑定（也能发现系统保留端口不可绑定），以及正在运行和已停止容器的 HostConfig.PortBindings。后端避开 8080–8090，18080 因其他已停止容器的预留映射而排除。

Docker 默认地址池已耗尽。未清理既有网络，也未修改 Docker daemon 配置；为本项目显式分配 10.253.81.0/24，经已有 Docker 网段与主机 IPv4 路由重叠检查通过。以后通过 CLOUDNEST_SUBNET 调整时，启动脚本会再次检查。

创建前后对比了 32 个既有容器的状态和 StartedAt，均保持一致。本项目的命令仅作用于 Compose project cloudnest。

## 镜像与来源

| 组件 | 固定来源 |
|---|---|
| MySQL | mysql:8.4.13，摘要 a81d043930e51745522bd5c430961cf8286f0a23f0a9a2a6e0108f7ea71b0902 |
| Redis | redis:7.4.12-alpine，摘要 8da19c7399dca900c6822a1cf30f9d2285585e2c4b457ff4095b0f4500054d3f |
| MinIO | 官方 RELEASE.2025-10-15T17-29-55Z，源码提交 9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a，本地构建 |
| MinIO 源码归档 | SHA256 45521908307306e925c98d629e1c17d78c8b72b6ee242b1bfb1409f7d8ee5841 |
| MinIO 构建工具链 | golang:1.27.2-alpine，摘要 f92b6ef800e499660581efdabdf25d9d817a9d124eaf900924f0504e7e27e12d |
| MinIO 运行底图 | alpine:3.23，摘要 ff7f0c727dd796aa36787bfcd9a64bf35a6ce72a73451b3359fde849b6703d63 |

MinIO 社区服务端已归档，本实例按技术方案仅作为本地学习环境。[官方仓库](https://github.com/minio/minio)

Docker 构建器直接拉取 GitHub 归档时出现 EOF；改由宿主机下载固定源码，宿主机和 Docker 构建中均校验 SHA256，再进行源码编译。归档属于可重新下载的构建缓存，不提交到 Git。

Wails 安装的首次依赖下载也遇到网络 EOF，重试时仅为该安装进程使用 Go 代理回退，校验数据库保持启用。未改动全局 GOPROXY 或当前项目的 Go 依赖。

## 认证与数据卷

本地开发默认账号密码为 root / root。Redis 使用命名 ACL 用户 root，禁用 default 用户，启用 AOF（appendfsync everysec）；未认证请求返回 NOAUTH。

MinIO 官方源码规定 secret key 最少 8 位，root 无法通过标准版本的凭证校验，因此使用 root / rootroot，不修改上游认证规则。[官方长度限制](https://github.com/minio/minio/blob/9e49d5e7a648f00e26f2246f4dc28e6b07f8c84a/internal/auth/credentials.go)

MySQL、Redis、MinIO 分别使用 cloudnest_mysql_data、cloudnest_redis_data、cloudnest_minio_data；停止容器保留数据。MySQL 数据库为 cloudnest，字符集 utf8mb4；MinIO 私有 Bucket 为 cloudnest。

## 已完成的验收

- Compose 配置解析、端口预检、子网重叠检查通过；三个服务均 healthy。
- MySQL 宿主机映射端口握手、容器网络 DNS、root 远程认证、建表/写入/查询/删除测试通过。
- Redis 宿主机映射端口、匿名访问拒绝、root ACL 登录及 SET/GET/DEL 测试通过。
- MinIO 真实 SigV4 认证、私有桶创建、两片 Multipart 上传、ListParts、完成、内容 SHA256、Range、AbortMultipartUpload 和匿名读取拒绝测试通过。
- MinIO 控制台 HTTP 200，返回 HTML 页面。
- 后端 go run 在 127.0.0.1:18081 启动，GET /ping 返回预期 JSON；验证后已结束临时后端进程。
- flutter doctor -v 和 wails3 doctor 通过，当前未做真机 APK、Windows 安装包或应用托盘 PoC。

组件资源上限分别为 MySQL 768 MiB、Redis 128 MiB、MinIO 512 MiB，避免本机开发实例无界占用资源。这是初始配置，后续大文件测试根据实测调整。
