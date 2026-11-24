import 'dart:async';

import 'package:flutter/services.dart';
import 'package:material_ui/material_ui.dart';

import 'cloud_drive_page.dart';
import 'login_page.dart';
import 'mobile_account.dart';
import 'mobile_tasks_page.dart';
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
  String? _documentScope;
  int _documentEpoch = 0;
  bool _restorePending = false;
  String? _activity;
  bool _needsReselection = false;
  _DocumentSummary? _document;
  String? _notice;

  @override
  void initState() {
    super.initState();
    unawaited(_runAccount(_account.restore));
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

  Future<void> _logout() => _runAccount(() async {
    _accountNotice = await _account.logout();
  });

  Future<void> _openCloudDrive() => _runAccount(() async {
    final document = _document;
    final notice = await Navigator.of(context).push<String>(
      MaterialPageRoute(
        builder: (context) => CloudDrivePage(
          account: _account,
          source: document == null
              ? null
              : (
                  name: document.name,
                  sizeBytes: document.sizeBytes,
                  sha256: document.sha256,
                ),
        ),
      ),
    );
    if (mounted) _accountNotice = notice;
  });

  Future<void> _openTaskRecords() => _runAccount(() async {
    final owner = _account.owner;
    if (owner == null) return;
    final notice = await Navigator.of(context).push<String>(
      MaterialPageRoute(
        builder: (context) => MobileTasksPage(owner: owner, account: _account),
      ),
    );
    if (mounted) _accountNotice = notice;
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
      if (mounted) {
        // 会话确定后再启用当前文件记录，访客与各账号有各自的范围。
        final owner = _account.owner;
        final scope = documentScopeFor(owner);
        setState(() {
          _accountBusy = false;
          if (_documentScope != scope) {
            _documentScope = scope;
            _documentEpoch++;
            _document = null;
            _notice = null;
            _needsReselection = false;
            _restorePending = true;
          }
        });
        _restoreCurrentDocument();
      }
    }
  }

  void _restoreCurrentDocument() {
    if (_restorePending && !_accountBusy && _activity == null) {
      unawaited(_readDocument(restore: true));
    }
  }

  Future<void> _readDocument({required bool restore}) async {
    final scope = _documentScope;
    if (_accountBusy || _activity != null || scope == null) return;
    final epoch = _documentEpoch;

    // 1. 启动时查找上次授权，主动点击时打开系统选择器。
    setState(() {
      _restorePending = false;
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
        scope,
      );

      // 2. 显示已计算的摘要和实际恢复方式。
      if (result != null) {
        document = _DocumentSummary.fromResult(result);
        notice = switch ((document.mode, restore)) {
          ('staged', true) => '已恢复本机暂存，可继续使用。',
          ('staged', false) => '已暂存到本机，可在重开应用后继续使用。',
          ('direct', true) => '已重新读取上次授权的文件。',
          _ => '上传源已准备，可直接读取。',
        };
      }
    } on PlatformException catch (error) {
      needsReselection =
          error.code == 'document_access_lost' ||
          error.code == 'document_restore_failed' ||
          error.code == 'document_source_changed';
      notice = switch (error.code) {
        'picker_unavailable' => '无法打开系统文件选择器。',
        'document_access_lost' => '上次文件的授权已失效，请重新选择。',
        'document_restore_failed' => '上次文件无法读取，请重新选择。',
        'document_source_changed' => '文件内容已变化或暂存已失效，请重新选择。',
        'document_staging_budget' => '暂存达到容量上限，已暂停准备。请选择更小或可直接读取的文件。',
        'document_staging_space' => '手机可用空间不足，已暂停准备。释放空间后请重新选择文件。',
        _ => '无法读取所选文件，请重新选择。',
      };
    } on MissingPluginException {
      if (!restore) notice = '当前设备无法打开系统文件选择器。';
    } on FormatException {
      notice = '文件读取结果无效，请重新选择。';
    }

    // 3. 只向仍在该范围的界面交付结果。切换后继续恢复新范围。
    if (!mounted) return;
    setState(() {
      _activity = null;
      if (epoch == _documentEpoch) {
        _needsReselection = needsReselection;
        _document = document;
        _notice = notice;
      }
    });
    _restoreCurrentDocument();
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
          else ...[
            TextButton(
              onPressed: _accountBusy ? null : _checkSession,
              child: const Text('核对会话'),
            ),
            TextButton(
              onPressed: _accountBusy ? null : _logout,
              child: const Text('退出登录'),
            ),
          ],
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
            if (username != null)
              Wrap(
                spacing: 12,
                runSpacing: 8,
                children: [
                  FilledButton(
                    onPressed: _accountBusy ? null : _openCloudDrive,
                    child: const Text('浏览云盘'),
                  ),
                  OutlinedButton(
                    onPressed: _accountBusy || activity != null
                        ? null
                        : _openTaskRecords,
                    child: const Text('任务记录'),
                  ),
                ],
              ),
            if (!_accountBusy && _documentScope != null) ...[
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
          ],
        ),
      ),
    );
  }
}

class _DocumentSummary {
  const _DocumentSummary(this.name, this.sizeBytes, this.sha256, this.mode);

  factory _DocumentSummary.fromResult(Map<String, Object?> result) {
    if (result
        case {
          'name': final String name,
          'sizeBytes': final int sizeBytes,
          'sha256': final String sha256,
          'sourceMode': final String mode,
        }
        when sizeBytes >= 0 && (mode == 'direct' || mode == 'staged')) {
      return _DocumentSummary(name, sizeBytes, sha256, mode);
    }
    throw const FormatException('Invalid Android document summary');
  }

  final String name;
  final int sizeBytes;
  final String sha256;
  final String mode;
}
