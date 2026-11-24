import 'dart:async';

import 'package:material_ui/material_ui.dart';

import 'app_theme.dart';
import 'cloud_drive_page.dart';
import 'login_page.dart';
import 'mobile_account.dart';
import 'native_login.dart';

class HomePage extends StatefulWidget {
  const HomePage({
    super.key,
    this.account,
    required this.themeMode,
    required this.onThemeChanged,
  });

  final MobileAccount? account;
  final ThemeMode themeMode;
  final Future<bool> Function(ThemeMode) onThemeChanged;

  @override
  State<HomePage> createState() => _HomePageState();
}

class _HomePageState extends State<HomePage> {
  late final _account = widget.account ?? MobileAccount();
  bool _accountBusy = false;
  String? _notice;

  @override
  void initState() {
    super.initState();
    unawaited(_runAccount(_account.restore));
  }

  Future<void> _openLogin() => _runAccount(() async {
    // 1. 只在会话安全写入后进入云盘。
    final session = await Navigator.of(context).push<NativeSession>(
      MaterialPageRoute(builder: (context) => const LoginPage()),
    );
    if (mounted && session != null) await _account.remember(session);
  });

  Future<void> _logout() async {
    await _runAccount(() async {
      _notice = await _account.logout();
    });
    final notice = _notice;
    if (mounted && _account.owner != null && notice != null) {
      ScaffoldMessenger.of(context)
          .showSnackBar(SnackBar(content: Text(notice)));
    }
  }

  Future<void> _chooseTheme() async {
    final chosen = await showAppThemeDialog(context, widget.themeMode);
    if (!mounted || chosen == null || chosen == widget.themeMode) return;
    final saved = await widget.onThemeChanged(chosen);
    if (!saved && mounted) {
      ScaffoldMessenger.of(context)
          .showSnackBar(const SnackBar(content: Text('当前主题已切换，但无法保存选择。')));
    }
  }

  Future<void> _runAccount(Future<void> Function() action) async {
    if (_accountBusy) return;
    setState(() {
      _accountBusy = true;
      _notice = null;
    });
    try {
      await action();
    } on AccountFailure catch (error) {
      _notice = error.message;
    } finally {
      if (mounted) setState(() => _accountBusy = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final owner = _account.owner;
    if (owner != null) {
      // 2. 切换账号时从各自根目录打开，丢弃上个账号的页面状态。
      return CloudDrivePage(
        key: ValueKey(owner),
        account: _account,
        accountBusy: _accountBusy,
        onLogout: _logout,
        onChooseTheme: _chooseTheme,
        onSessionEnded: (message) => setState(() => _notice = message),
      );
    }
    return Scaffold(
      appBar: AppBar(
        title: const Text('CloudNest'),
        actions: [
          IconButton(
            tooltip: '界面主题',
            onPressed: _chooseTheme,
            icon: const Icon(Icons.contrast),
          ),
          TextButton(
            onPressed: _accountBusy ? null : _openLogin,
            child: const Text('登录'),
          ),
        ],
      ),
      body: SafeArea(
        child: ListView(
          padding: const EdgeInsets.all(24),
          children: [
            if (_accountBusy) const LinearProgressIndicator(),
            Semantics(
              liveRegion: true,
              child: Text(_notice ?? (_accountBusy ? '正在恢复会话…' : '登录后即可浏览云盘。')),
            ),
          ],
        ),
      ),
    );
  }
}
