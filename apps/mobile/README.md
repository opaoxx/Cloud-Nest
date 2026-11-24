# CloudNest Android

**连接前，Android 手机或平板必须和运行 CloudNest 后端的电脑处于同一个可互通的局域网。**首次准备好 Docker、存储桶和业务账号后，在电脑的仓库根目录用管理员 PowerShell 执行 `./scripts/Start-LanTrial.ps1`。

打开调试 APK，对照终端的 `Phone/tablet service address` 将电脑 IPv4 的四段数字和端口分别填入登录页。默认业务用户名和密码均为 `root`，首次初始化时自定义过密码则使用该密码。使用结束后在电脑上执行 `./scripts/Stop-LanTrial.ps1`。[完整启动步骤和填写示例](../../README.md#客户端登录与后端启停)

## 构建调试 APK

项目使用 `.fvmrc` 固定的 Flutter 3.50.1 稳定版和配套的 Dart 3.14.0。Android 工具链需要 API 36 平台、NDK 28.2.13676358 和可运行 Gradle 9.5.0 的 JDK。

在此目录执行。

```powershell
flutter --version
flutter pub get
flutter analyze
flutter build apk --debug
```

生成文件位于 `build/app/outputs/flutter-apk/app-debug.apk`。应用 ID 为 `com.opaoxx.cloudnest`，最低 Android API 为 24。调试 APK 使用 Android 调试签名。
