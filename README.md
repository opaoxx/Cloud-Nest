# CloudNest

CloudNest 把文件保存在你的 Windows 电脑上，供 Windows 客户端、Android 手机和平板或本机浏览器访问。

**连接前请先确认：运行后端的电脑与手机、平板必须处于同一个可互通的局域网。** 电脑可用网线连接路由器，手机和平板连接同一路由器的 Wi-Fi。使用期间保持电脑开机、后端运行；访客 Wi-Fi 若开启设备隔离，即使连上同一个 Wi-Fi 也可能无法访问电脑。

## 先选数据盘

首次启动前，在 Docker Desktop 的 **Settings → Resources → Advanced** 将 WSL 2 数据磁盘位置设到空间充足的非系统盘。上传的文件会持续占用这块盘的空间。开发机上的实际磁盘映像为 `F:\Docker\wsl\DockerDesktopWSL\disk\docker_data.vhdx`，你的电脑以自己的 Docker 设置为准。[Docker Desktop 存储说明](https://docs.docker.com/desktop/features/wsl/)

| 数据 | 保存位置 |
| --- | --- |
| 上传的文件正文 | MinIO 命名卷 `cloudnest_minio_data`，挂载在容器的 `/data` |
| 文件目录、账号、配额和任务记录 | MySQL 命名卷 `cloudnest_mysql_data`，挂载在容器的 `/var/lib/mysql` |
| 上传临时文件 | 局域网启动脚本使用仓库下的 `output/lan-trial/uploads-temp`。建议把仓库也放在非系统盘。 |

备份网盘时应同时保留 MinIO 和 MySQL 数据。日常停止后端请使用下方停止脚本，`docker compose down -v` 会删除项目命名卷。

## 首次构建和初始化

在部署电脑上准备 Go 1.27.2、Python 3.11 或更新版本、PowerShell 和 Docker Desktop（WSL 2、Linux 容器）。打开 Docker Desktop，在仓库根目录执行以下命令。

```powershell
./scripts/Start-Infra.ps1
python scripts/verify_infra.py --service minio
go run ./cmd/database -database cloudnest -seed
```

第一条命令构建 MinIO 镜像并启动 MySQL、MinIO 和 Redis，第二条准备私有存储桶并验证读写，第三条应用数据库迁移、创建业务账号和根目录。默认用户名和密码均为 `root`。如需自定义密码，在**首次**执行 `-seed` 前设置 `CLOUDNEST_ADMIN_PASSWORD`，已有账号沿用原密码。

已有第二版试用安装包的用户可直接安装。需要从源码构建时，按设备选择：

- **Windows x64**：安装 NSIS，将 `makensis.exe` 加入 PATH，在仓库根目录运行 `./scripts/Build-WindowsPreview.ps1 -PreviewRevision 2`。安装包输出到 `output/desktop-preview.2`，客户端运行需要 WebView2 Evergreen Runtime。
- **Android 手机或平板**：使用 Flutter 3.50.1 构建调试 APK，将 APK 安装到设备上。工具链、构建命令和输出路径见 [Android 构建说明](apps/mobile/README.md)。

## 客户端登录与后端启停

**1. 在电脑上启动后端。** 打开 Docker Desktop，用**管理员 PowerShell** 进入仓库根目录，执行：

```powershell
./scripts/Start-LanTrial.ps1
```

脚本编译并在后台启动 API，设置当前子网使用的端口转发和防火墙规则。启动成功后，终端会打印以下三行。示例中的 `192.168.1.23` 请换成**你当次终端显示的 IP**。

```text
Phone/tablet service address: http://192.168.1.23:18081
Windows service address: http://127.0.0.1:18081
File endpoint: http://192.168.1.23:19000
```

**2. 在客户端登录。** 首次默认填写**用户名 `root`、密码 `root`**。初始化时自定义过密码的，填写自定义密码。

| 客户端 | 登录页如何填地址 |
| --- | --- |
| 运行后端的 Windows 电脑 | 在「服务连接」填 `Windows service address` 的完整 URL，默认是 `http://127.0.0.1:18081`。可先点「检查连接」。 |
| 同一局域网内的 Android 手机或平板 | 按 `Phone/tablet service address` 将 IPv4 四段数字分别填入地址框。以上例为例，依次填 `192`、`168`、`1`、`23`，端口填 `18081`。调试 APK 自动补上协议。 |

`File endpoint` 用于文件传输。登录成功后，可以从手机上传一个小文件，在 Windows 客户端打开同一目录，点击「刷新目录」后下载。

**3. 使用结束后停止后端。** 关闭启动终端后，后台 API 仍会运行。在仓库根目录用管理员 PowerShell 执行：

```powershell
./scripts/Stop-LanTrial.ps1
```

停止脚本关闭后台 API 并清理本次端口转发和防火墙规则，Docker 基础容器继续运行。切换 Wi-Fi 后，让设备重新连接同一局域网，再运行启动脚本并按新的终端输出填写地址。

## 可选：在部署电脑上运行 Web

启动后端后，在另一个 PowerShell 中执行以下命令。前端使用 Node.js 24 和 pnpm 11.21.0。

```powershell
pnpm --dir apps/web install --frozen-lockfile
$env:VITE_CLOUDNEST_DATA_MODE = 'api'
pnpm --dir apps/web dev
```

打开 [http://127.0.0.1:5173](http://127.0.0.1:5173)，使用同一个业务账号登录。需要生产构建时，在上述环境变量生效的终端运行 `pnpm --dir apps/web build`，产物位于 `apps/web/dist`，部署时将同源 `/api` 转发给 Go 后端。
