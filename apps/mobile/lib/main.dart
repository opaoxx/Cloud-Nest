import 'dart:async';

import 'package:material_ui/material_ui.dart';

import 'app_theme.dart';
import 'home_page.dart';
import 'mobile_account.dart';

void main() {
  // 1. 交给 Flutter 驱动应用的根组件。
  runApp(const CloudNestApp());
}

class CloudNestApp extends StatefulWidget {
  const CloudNestApp({super.key, this.account});

  final MobileAccount? account;

  @override
  State<CloudNestApp> createState() => _CloudNestAppState();
}

class _CloudNestAppState extends State<CloudNestApp> {
  static const _seed = Color(0xFF26745D);
  ThemeMode _mode = ThemeMode.system;
  bool _chosen = false;

  @override
  void initState() {
    super.initState();
    unawaited(_restoreTheme());
  }

  Future<void> _restoreTheme() async {
    // 1. 读取本机偏好。若用户已选择，则保留更新的选择。
    final mode = await readAppTheme();
    if (mounted && !_chosen) setState(() => _mode = mode);
  }

  Future<bool> _setTheme(ThemeMode mode) {
    // 2. 全部页面即时换色，再保存到设备本地。
    setState(() {
      _chosen = true;
      _mode = mode;
    });
    return saveAppTheme(mode);
  }

  @override
  Widget build(BuildContext context) {
    // 3. 系统模式自动使用系统明暗，手动选择则沿用指定配色。
    return MaterialApp(
      title: 'CloudNest',
      debugShowCheckedModeBanner: false,
      theme: ThemeData(colorSchemeSeed: _seed),
      darkTheme: ThemeData(colorSchemeSeed: _seed, brightness: Brightness.dark),
      themeMode: _mode,
      home: HomePage(
        account: widget.account,
        themeMode: _mode,
        onThemeChanged: _setTheme,
      ),
    );
  }
}
