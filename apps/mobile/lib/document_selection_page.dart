import 'dart:async';

import 'package:flutter/services.dart';
import 'package:material_ui/material_ui.dart';

import 'login_page.dart';
import 'mobile_account.dart';
import 'native_login.dart';

const _documentPicker = MethodChannel('cloudnest/android_document');

class DocumentSelectionPage extends StatefulWidget {
  const DocumentSelectionPage({super.key});

  @override
  State<DocumentSelectionPage> createState() => _DocumentSelectionPageState();
}

class _DocumentSelectionPageState extends State<DocumentSelectionPage> {
  final _account = MobileAccount();
  bool _accountBusy = false;
  String? _accountNotice;
  String? _activity;
  bool _needsReselection = false;
  _DocumentSummary? _document;
  String? _notice;

  @override
  void initState() {
    super.initState();
    unawaited(_runAccount(_account.restore));
    unawaited(_readDocument(restore: true));
  }

  Future<void> _openLogin() => _runAccount(() async {
    final session = await Navigator.of(context).push<NativeSession>(
      MaterialPageRoute(builder: (context) => const LoginPage()),
    );
    if (mounted && session != null) await _account.remember(session);
  });

  Future<void> _checkSession() => _runAccount(() async {
    final username = await _account.readProfile();
    _accountNotice = '已核对账号 $username';
  });

  Future<void> _runAccount(Future<void> Function() action) async {
    if (_accountBusy) return;
    setState(() {
      _accountBusy = true;
      _accountNotice = null;
    });
    try {
      await action();
    } on AccountFailure catch (error) {
      _accountNotice = error.message;
    } finally {
      if (mounted) setState(() => _accountBusy = false);
    }
  }

  Future<void> _readDocument({required bool restore}) async {
    if (_activity != null) return;

    // 1. 启动时查找上次授权，主动点击时打开系统选择器。
    setState(() {
      _activity = restore ? '正在检查上次文件授权…' : '正在选择或读取文件…';
      _document = null;
      _notice = null;
    });

    _DocumentSummary? document;
    String? notice;
    var needsReselection = false;
    try {
      final result = await _documentPicker.invokeMapMethod<String, Object?>(
        restore ? 'restoreDocument' : 'pickAndSummarize',
      );

      // 2. 只展示 Android 刚读取到的内容，并提示授权能否跨重启。
      if (result != null) {
        document = _DocumentSummary.fromResult(result);
        if (restore) {
          notice = '已重新读取上次授权的文件。';
        } else if (!document.persisted) {
          notice = '已读取文件。本次授权无法在重启后恢复。';
        }
      }
    } on PlatformException catch (error) {
      needsReselection =
          error.code == 'document_access_lost' ||
          error.code == 'document_restore_failed';
      notice = switch (error.code) {
        'picker_unavailable' => '无法打开系统文件选择器。',
        'document_access_lost' => '上次文件的授权已失效，请重新选择。',
        'document_restore_failed' => '上次文件无法读取，请重新选择。',
        _ => '无法读取所选文件，请重新选择。',
      };
    } on MissingPluginException {
      if (!restore) notice = '当前设备无法打开系统文件选择器。';
    } on FormatException {
      notice = '文件读取结果无效，请重新选择。';
    }

    // 3. 授权失效时恢复选择入口。取消继续回到空闲。
    if (!mounted) return;
    setState(() {
      _activity = null;
      _needsReselection = needsReselection;
      _document = document;
      _notice = notice;
    });
  }

  @override
  Widget build(BuildContext context) {
    final document = _document;
    final notice = _notice;
    final activity = _activity;
    final username = _account.username;
    final accountNotice = _accountNotice;
    return Scaffold(
      appBar: AppBar(
        title: const Text('CloudNest'),
        actions: [
          if (username == null)
            TextButton(
              onPressed: _accountBusy ? null : _openLogin,
              child: const Text('登录'),
            )
          else
            TextButton(
              onPressed: _accountBusy ? null : _checkSession,
              child: const Text('核对会话'),
            ),
        ],
      ),
      body: SafeArea(
        child: ListView(
          padding: const EdgeInsets.all(24),
          children: [
            if (username != null)
              Padding(
                padding: const EdgeInsets.only(bottom: 16),
                child: Text('已登录为 $username'),
              ),
            if (_accountBusy && username == null) const Text('正在处理会话…'),
            if (accountNotice != null)
              Padding(
                padding: const EdgeInsets.only(bottom: 16),
                child: Semantics(liveRegion: true, child: Text(accountNotice)),
              ),
            Align(
              alignment: Alignment.centerLeft,
              child: FilledButton(
                onPressed: activity == null
                    ? () => _readDocument(restore: false)
                    : null,
                child: Text(_needsReselection ? '重新选择文件' : '选择文件'),
              ),
            ),
            if (activity != null) Text(activity),
            if (notice != null) Text(notice),
            if (document != null) ...[
              const SizedBox(height: 20),
              Text(document.name),
              Text('大小 ${document.sizeBytes} B'),
              Text('SHA-256 ${document.sha256}'),
            ],
          ],
        ),
      ),
    );
  }
}

class _DocumentSummary {
  const _DocumentSummary(
    this.name,
    this.sizeBytes,
    this.sha256,
    this.persisted,
  );

  factory _DocumentSummary.fromResult(Map<String, Object?> result) {
    if (result
        case {
          'name': final String name,
          'sizeBytes': final int sizeBytes,
          'sha256': final String sha256,
          'persisted': final bool persisted,
        }
        when sizeBytes >= 0) {
      return _DocumentSummary(name, sizeBytes, sha256, persisted);
    }
    throw const FormatException('Invalid Android document summary');
  }

  final String name;
  final int sizeBytes;
  final String sha256;
  final bool persisted;
}
