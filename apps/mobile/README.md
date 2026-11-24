# CloudNest Android

安装 APK 后，从桌面打开 CloudNest。首页显示应用名称。

## 构建调试 APK

项目使用 `.fvmrc` 固定的 Flutter 3.50.1 稳定版和配套的 Dart 3.14.0。Android 工具链需要 API 36 平台、NDK 28.2.13676358 和可运行 Gradle 9.5.0 的 JDK。

在此目录执行。

```powershell
flutter --version
flutter pub get
flutter analyze
flutter test
flutter build apk --debug
```

生成文件位于 `build/app/outputs/flutter-apk/app-debug.apk`。应用 ID 为 `com.opaoxx.cloudnest`，最低 Android API 为 24。调试 APK 使用 Android 调试签名。
