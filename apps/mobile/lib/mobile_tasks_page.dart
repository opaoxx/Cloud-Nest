import 'dart:async';

import 'package:material_ui/material_ui.dart';

import 'file_presentation.dart';
import 'mobile_account.dart';
import 'mobile_tasks.dart';
import 'native_login.dart';
import 'native_multipart_upload.dart';
import 'transfer_pause.dart';

class MobileTasksPage extends StatefulWidget {
  const MobileTasksPage({
    super.key,
    required this.owner,
    required this.account,
  });

  final AccountOwner owner;
  final MobileAccount account;

  @override
  State<MobileTasksPage> createState() => _MobileTasksPageState();
}

class _MobileTasksPageState extends State<MobileTasksPage> {
  List<MobileTask> _tasks = [];
  bool _busy = false;
  bool _loadFailed = false;
  String? _activeId;
  TransferPause? _pause;
  String? _notice;
  final _sourceRequired = <String>{};

  @override
  void initState() {
    super.initState();
    unawaited(_load());
  }

  Future<void> _resume(MobileTask task) async {
    if (_activeId != null || _busy) return;
    final pause = TransferPause();
    setState(() {
      _activeId = task.id;
      _pause = pause;
      _notice = '正在核对原任务和文件…';
    });
    try {
      final completed = task.isDownload
          ? await widget.account.resumeDownload(
              widget.owner,
              task.id,
              pause: pause,
            )
          : await widget.account.resumeMultipart(
              widget.owner,
              task.id,
              reselect:
                  _sourceRequired.contains(task.id) ||
                  task.pauseReason == 'source',
              pause: pause,
              onProgress: (plan, confirmed) {
                if (mounted) {
                  setState(() {
                    _sourceRequired.remove(task.id);
                    _notice = uploadProgressLabel(confirmed, plan.partCount);
                  });
                }
              },
            );
      if (!mounted) return;
      setState(() {
        if (completed != null) _sourceRequired.remove(task.id);
        _notice = task.isDownload
            ? (completed == null ? '已取消保存。' : '已保存「$completed」。')
            : (completed == null ? '已取消重新选择。任务保持暂停。' : '上传成功。');
      });
    } on MultipartSourceUnavailable catch (error) {
      if (mounted) {
        setState(() {
          _sourceRequired.add(task.id);
          _notice = '已暂停。\n${error.message}';
        });
      }
    } on AccountFailure catch (error) {
      if (!mounted) return;
      if (widget.account.owner != widget.owner) {
        Navigator.of(context).pop(error.message);
        return;
      }
      setState(() => _notice = error.message);
    } finally {
      if (mounted) {
        setState(() {
          _activeId = null;
          _pause = null;
        });
      }
    }
    if (mounted) await _load();
  }

  Future<void> _load() async {
    if (_busy) return;
    setState(() {
      _busy = true;
      _loadFailed = false;
    });
    try {
      // 1. 打开或恢复任务后，读取当前账号的真实上传和下载记录。
      final tasks = await readMobileTasks(widget.owner);
      if (mounted) setState(() => _tasks = tasks);
    } on TaskStoreFailure {
      // 2. 本机存储暂时不可读时仍可重试。
      if (mounted) setState(() => _loadFailed = true);
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  Future<void> _cancel(MobileTask task) async {
    if (_activeId != null || _busy) return;
    final submit = task.state != 'CANCELLING';
    if (submit) {
      // 只有确认按钮会写取消意图。关闭对话框保留任务及进度。
      final accepted = await showDialog<bool>(
        context: context,
        builder: (dialogContext) => AlertDialog(
          title: Text(task.isDownload ? '取消下载' : '取消上传'),
          content: Text(
            task.isDownload
                ? '取消「${task.name}」会清理本机下载断点。'
                : '取消「${task.name}」后，将查询服务端的实际结果。',
          ),
          actions: [
            TextButton(
              onPressed: () => Navigator.of(dialogContext).pop(false),
              child: const Text('保留任务'),
            ),
            FilledButton(
              onPressed: () => Navigator.of(dialogContext).pop(true),
              child: const Text('确认取消'),
            ),
          ],
        ),
      );
      if (accepted != true || !mounted || _activeId != null) return;
    }
    setState(() {
      _activeId = task.id;
      _notice = submit ? '正在确认取消…' : '正在查询取消结果…';
    });
    try {
      final result = await widget.account.cancelTransfer(
        widget.owner,
        task.id,
        submit: submit,
      );
      if (mounted) setState(() => _notice = result);
    } on AccountFailure catch (error) {
      if (!mounted) return;
      if (widget.account.owner != widget.owner) {
        Navigator.of(context).pop(error.message);
        return;
      }
      setState(() => _notice = error.message);
    } finally {
      if (mounted) setState(() => _activeId = null);
    }
    if (mounted) await _load();
  }

  @override
  Widget build(BuildContext context) {
    final notice = _notice;
    final pause = _pause;
    return PopScope(
      canPop: _activeId == null,
      child: Scaffold(
        appBar: AppBar(
          title: const Text('任务记录'),
          actions: [
            if (pause != null)
              TextButton(
                onPressed: pause.requested
                    ? null
                    : () => setState(pause.request),
                child: Text(pause.requested ? '正在暂停…' : '暂停'),
              ),
          ],
        ),
        body: SafeArea(
          child: Column(
            children: [
              if (_activeId != null) const LinearProgressIndicator(),
              if (notice != null)
                Padding(
                  padding: const EdgeInsets.all(12),
                  child: Semantics(liveRegion: true, child: Text(notice)),
                ),
              Expanded(child: _body(context)),
            ],
          ),
        ),
      ),
    );
  }

  Widget _body(BuildContext context) {
    if (_busy) return const Center(child: CircularProgressIndicator());
    if (_loadFailed) {
      return _message('无法读取任务记录。', '重试', _load);
    }
    if (_tasks.isEmpty) {
      return const Center(child: Text('暂无任务记录'));
    }

    final dates = MaterialLocalizations.of(context);
    return ListView.builder(
      itemCount: _tasks.length,
      itemBuilder: (context, index) {
        final task = _tasks[index];
        final date = dates.formatCompactDate(task.createdAt);
        final time = dates.formatTimeOfDay(
          TimeOfDay.fromDateTime(task.createdAt),
        );
        final sourceRequired =
            _sourceRequired.contains(task.id) || task.pauseReason == 'source';
        final details = [
          if (task.sizeBytes case final int size) fileSizeLabel(size),
          if (_activeId == task.id)
            '正在处理…'
          else ...[
            if (sourceRequired && task.canResume && task.pauseReason == null)
              '已暂停',
            task.statusLabel,
          ],
        ];
        return ListTile(
          key: ValueKey(task.id),
          leading: Icon(
            task.isDownload ? Icons.download_outlined : Icons.upload_outlined,
          ),
          title: Text(task.name, maxLines: 2, overflow: TextOverflow.ellipsis),
          subtitle: Text("${details.join(' · ')}\n$date $time"),
          isThreeLine: true,
          trailing: _taskActions(task, sourceRequired),
        );
      },
    );
  }

  Widget? _taskActions(MobileTask task, bool sourceRequired) {
    final idle = _activeId == null;
    if (task.state == 'CANCEL_PENDING' || task.state == 'CANCELLING') {
      return TextButton(
        onPressed: idle ? () => _cancel(task) : null,
        child: Text(task.state == 'CANCELLING' ? '查看取消结果' : '重试取消'),
      );
    }
    if (!task.canResume && !task.canCancel) return null;
    return Row(
      mainAxisSize: MainAxisSize.min,
      children: [
        if (task.canResume)
          TextButton(
            onPressed: idle ? () => _resume(task) : null,
            child: Text(
              task.isDownload ? '继续下载' : (sourceRequired ? '重新选择文件' : '继续上传'),
            ),
          ),
        if (task.canCancel)
          IconButton(
            tooltip: '取消${task.isDownload ? '下载' : '上传'} ${task.name}',
            onPressed: idle ? () => _cancel(task) : null,
            icon: const Icon(Icons.cancel_outlined),
          ),
      ],
    );
  }

  Widget _message(String message, String actionText, VoidCallback onPressed) {
    return Center(
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: [
          Semantics(liveRegion: true, child: Text(message)),
          const SizedBox(height: 12),
          FilledButton(onPressed: onPressed, child: Text(actionText)),
        ],
      ),
    );
  }
}
