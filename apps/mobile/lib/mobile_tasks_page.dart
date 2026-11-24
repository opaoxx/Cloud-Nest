import 'dart:async';

import 'package:material_ui/material_ui.dart';

import 'mobile_account.dart';
import 'mobile_tasks.dart';

class MobileTasksPage extends StatefulWidget {
  const MobileTasksPage({super.key, required this.owner});

  final AccountOwner owner;

  @override
  State<MobileTasksPage> createState() => _MobileTasksPageState();
}

class _MobileTasksPageState extends State<MobileTasksPage> {
  List<MobileTask> _tasks = [];
  bool _busy = false;
  bool? _failedCreation;

  @override
  void initState() {
    super.initState();
    unawaited(_load());
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
    return Scaffold(
      appBar: AppBar(title: const Text('任务记录')),
      body: SafeArea(child: _body(context)),
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
        return ListTile(
          key: ValueKey(task.id),
          leading: const Icon(Icons.assignment_outlined),
          title: Text(task.name),
          subtitle: Text('${task.statusLabel} · $date $time'),
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
