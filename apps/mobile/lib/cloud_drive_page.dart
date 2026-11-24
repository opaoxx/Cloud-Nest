import 'dart:async';

import 'package:material_ui/material_ui.dart';

import 'mobile_account.dart';
import 'native_directory.dart';
import 'native_login.dart';

class CloudDrivePage extends StatefulWidget {
  const CloudDrivePage({super.key, required this.account});

  final MobileAccount account;

  @override
  State<CloudDrivePage> createState() => _CloudDrivePageState();
}

class _CloudDrivePageState extends State<CloudDrivePage> {
  String? _rootId;
  List<DirectoryBreadcrumb> _breadcrumbs = [];
  List<DirectoryEntry> _entries = [];
  String? _nextCursor;
  bool _loading = false;
  ({String message, String? parentId, String? cursor})? _failure;

  String? get _currentDirectory =>
      _breadcrumbs.isEmpty ? null : _breadcrumbs.last.id;

  @override
  void initState() {
    super.initState();
    unawaited(_load(null));
  }

  Future<void> _load(String? parentId, {String? cursor}) async {
    if (_loading) return;
    setState(() {
      _loading = true;
      _failure = null;
    });

    try {
      // 1. 首次从本人资料取得根目录 ID，导航和续页使用实际目录 ID。
      final rootId =
          _rootId ?? (await widget.account.currentProfile()).rootEntryId;
      if (!mounted || ModalRoute.of(context)?.isCurrent != true) return;
      final listing = await widget.account.listDirectory(
        rootId: rootId,
        parentId: parentId ?? rootId,
        cursor: cursor,
      );
      if (!mounted || ModalRoute.of(context)?.isCurrent != true) return;

      // 2. 翻页按条目 ID 合并。切换目录或刷新成功后，从新首页开始。
      final byId = {
        if (cursor != null)
          for (final entry in _entries) entry.id: entry,
        for (final entry in listing.entries) entry.id: entry,
      };
      setState(() {
        _rootId = rootId;
        _entries = byId.values.toList();
        _breadcrumbs = listing.breadcrumbs;
        _nextCursor = listing.nextCursor;
      });
    } on AccountFailure catch (error) {
      // 3. 保留上次成功的列表。远端会话结束时回首页显示重新登录提示。
      if (!mounted || ModalRoute.of(context)?.isCurrent != true) return;
      if (widget.account.owner == null) {
        Navigator.of(context).pop(error.message);
      } else {
        setState(() {
          _failure = (
            message: error.message,
            parentId: parentId,
            cursor: cursor,
          );
        });
      }
    } finally {
      if (mounted) setState(() => _loading = false);
    }
  }

  Widget _pathBar() {
    final path = Row(
      children: [
        for (var index = 0; index < _breadcrumbs.length; index++) ...[
          if (index > 0) const Icon(Icons.chevron_right, size: 18),
          if (index == _breadcrumbs.length - 1)
            Padding(
              padding: const EdgeInsets.all(12),
              child: Text(index == 0 ? '根目录' : _breadcrumbs[index].name),
            )
          else
            TextButton(
              onPressed: _loading ? null : () => _load(_breadcrumbs[index].id),
              child: Text(index == 0 ? '根目录' : _breadcrumbs[index].name),
            ),
        ],
      ],
    );
    return Padding(
      padding: const EdgeInsets.symmetric(horizontal: 12),
      child: LayoutBuilder(
        builder: (context, constraints) => SingleChildScrollView(
          scrollDirection: Axis.horizontal,
          reverse: true,
          // 短路径对齐左侧，超出宽度时从当前目录这一端开始显示。
          child: ConstrainedBox(
            constraints: BoxConstraints(minWidth: constraints.maxWidth),
            child: path,
          ),
        ),
      ),
    );
  }

  Widget _entryList() {
    if (_entries.isEmpty) {
      if (_breadcrumbs.isEmpty && !_loading) return const SizedBox.shrink();
      return Center(child: Text(_breadcrumbs.isEmpty ? '正在读取云盘目录…' : '此目录为空'));
    }
    return ListView.builder(
      key: ValueKey(_currentDirectory),
      itemCount: _entries.length,
      itemBuilder: (context, index) {
        final entry = _entries[index];
        return ListTile(
          leading: Icon(
            entry.isFolder ? Icons.folder_outlined : Icons.description_outlined,
          ),
          title: Text(entry.name, maxLines: 2, overflow: TextOverflow.ellipsis),
          subtitle: entry.isFolder ? null : Text('${entry.sizeBytes} B'),
          trailing: entry.isFolder ? const Icon(Icons.chevron_right) : null,
          onTap: entry.isFolder && !_loading ? () => _load(entry.id) : null,
        );
      },
    );
  }

  @override
  Widget build(BuildContext context) {
    final failure = _failure;
    final cursor = _nextCursor;
    return Scaffold(
      appBar: AppBar(
        title: const Text('云盘'),
        actions: [
          IconButton(
            tooltip: '刷新目录',
            onPressed: _loading ? null : () => _load(_currentDirectory),
            icon: const Icon(Icons.refresh),
          ),
        ],
      ),
      body: SafeArea(
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: [
            if (_breadcrumbs.isNotEmpty) _pathBar(),
            if (_loading) const LinearProgressIndicator(),
            if (failure != null)
              Padding(
                padding: const EdgeInsets.symmetric(horizontal: 16),
                child: Row(
                  children: [
                    Expanded(
                      child: Semantics(
                        liveRegion: true,
                        child: Text(failure.message),
                      ),
                    ),
                    TextButton(
                      onPressed: () =>
                          _load(failure.parentId, cursor: failure.cursor),
                      child: const Text('重试'),
                    ),
                  ],
                ),
              ),
            Expanded(child: _entryList()),
            if (cursor != null)
              Padding(
                padding: const EdgeInsets.fromLTRB(16, 8, 16, 12),
                child: OutlinedButton(
                  onPressed: _loading
                      ? null
                      : () => _load(_currentDirectory, cursor: cursor),
                  child: const Text('加载更多'),
                ),
              ),
          ],
        ),
      ),
    );
  }
}
