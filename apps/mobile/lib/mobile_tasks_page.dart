import 'dart:async';

import 'package:material_ui/material_ui.dart';

import 'mobile_account.dart';
import 'mobile_tasks.dart';
import 'native_login.dart';
import 'native_multipart_upload.dart';

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
  bool? _failedCreation;
  String? _activeId;
  String? _notice;
  final _sourceRequired = <String>{};

  @override
  void initState() {
    super.initState();
    unawaited(_load());
  }

  Future<void> _resume(MobileTask task) async {
    if (_activeId != null || _busy) return;
    setState(() {
      _activeId = task.id;
      _notice = '正在核对原任务和文件…';
    });
    try {
      final published = await widget.account.resumeMultipart(
        widget.owner,
        task.id,
        reselect: _sourceRequired.contains(task.id),
        onProgress: (plan, confirmed) {
          if (mounted) {
            setState(() {
              _sourceRequired.remove(task.id);
              _notice = confirmed == plan.partCount
                  ? '分片已确认，等待校验和发布…'
                  : '已确认 $confirmed/${plan.partCount} 片';
            });
          }
        },
      );
      if (!mounted) return;
      setState(() {
        if (published != null) _sourceRequired.remove(task.id);
        _notice = published == null ? '已取消重新选择。任务保持暂停。' : '上传成功。';
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
      if (mounted) setState(() => _activeId = null);
    }
    if (mounted) await _load();
  }

  Future<void> _load({bool create = false}) async {
    if (_busy) return;
    setState(() {
      _busy = true;
      _failedCreation = null;
    });
    try {
      // 1. 打开页面时读当前账号。用户点击后才执行创建。
      final tasks = create
          ? await createMobileTestTask(widget.owner)
          : await readMobileTasks(widget.owner);
      if (mounted) setState(() => _tasks = tasks);
    } on TaskStoreFailure {
      // 2. 保留操作类型，重试仍走原来的读或写。
      if (mounted) setState(() => _failedCreation = create);
    } finally {
      if (mounted) setState(() => _busy = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final notice = _notice;
    return PopScope(
      canPop: _activeId == null,
      child: Scaffold(
        appBar: AppBar(title: const Text('任务记录')),
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
    final failedCreation = _failedCreation;
    if (failedCreation != null) {
      return _message(
        failedCreation ? '无法创建测试任务。' : '无法读取任务记录。',
        '重试',
        () => _load(create: failedCreation),
      );
    }
    if (_tasks.isEmpty) {
      return _message('暂无任务记录', '创建测试任务', () => _load(create: true));
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
        final sourceRequired = _sourceRequired.contains(task.id);
        return ListTile(
          key: ValueKey(task.id),
          leading: const Icon(Icons.assignment_outlined),
          title: Text(task.name),
          subtitle: Text(
            '${sourceRequired && task.canResume ? '已暂停 · ' : ''}${task.statusLabel} · $date $time',
          ),
          trailing: task.canResume
              ? TextButton(
                  onPressed: _activeId == null ? () => _resume(task) : null,
                  child: Text(sourceRequired ? '重新选择文件' : '继续上传'),
                )
              : null,
        );
      },
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
