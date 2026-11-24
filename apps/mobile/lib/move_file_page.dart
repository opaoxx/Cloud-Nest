import 'dart:async';

import 'package:material_ui/material_ui.dart';

import 'mobile_account.dart';
import 'native_directory.dart';
import 'native_login.dart';

class MoveFilePage extends StatefulWidget {
  const MoveFilePage({
    super.key,
    required this.account,
    required this.rootId,
    required this.sourceParentId,
    required this.file,
  });

  final MobileAccount account;
  final String rootId;
  final String sourceParentId;
  final DirectoryEntry file;

  @override
  State<MoveFilePage> createState() => _MoveFilePageState();
}

class _MoveFilePageState extends State<MoveFilePage> {
  DirectoryListing? _listing;
  bool _loading = false;
  bool _submitting = false;
  (String, String?)? _failedPage;
  (String, String)? _attempt;
  String? _notice;

  bool get _busy => _loading || _submitting;

  @override
  void initState() {
    super.initState();
    unawaited(_open(widget.rootId));
  }

  Future<void> _open(String parentId, [String? cursor]) async {
    if (_busy) return;
    setState(() {
      _loading = true;
      _failedPage = null;
      _notice = null;
    });
    try {
      // 1. 复用账号的目录分页，只在选择器展示文件夹。
      final page = await widget.account.listDirectory(
        rootId: widget.rootId,
        parentId: parentId,
        cursor: cursor,
      );
      if (!mounted) return;
      final folders = {
        if (cursor != null)
          for (final entry in _listing!.entries) entry.id: entry,
        for (final entry in page.entries)
          if (entry.isFolder) entry.id: entry,
      };
      setState(() {
        _listing = (
          breadcrumbs: page.breadcrumbs,
          entries: folders.values.toList(),
          nextCursor: page.nextCursor,
        );
      });
    } on AccountFailure catch (error) {
      if (!mounted) return;
      if (widget.account.owner == null) {
        Navigator.of(context).pop();
      } else {
        setState(() {
          _failedPage = (parentId, cursor);
          _notice = error.message;
        });
      }
    } finally {
      if (mounted) setState(() => _loading = false);
    }
  }

  Future<void> _move(String targetId) async {
    if (_busy || targetId == widget.sourceParentId || _failedPage != null) {
      return;
    }
    final previous = _attempt;
    final attempt = previous != null && previous.$1 == targetId
        ? previous
        : (targetId, newFileActionKey());
    setState(() {
      _submitting = true;
      _attempt = attempt;
      _notice = null;
    });
    try {
      // 2. 以原文件修订提交，重试同一位置时沿用原请求键。
      final moved = await widget.account.moveFile(
        widget.file,
        widget.sourceParentId,
        targetId,
        attempt.$2,
      );
      if (mounted) Navigator.of(context).pop(moved);
    } on AccountFailure catch (error) {
      if (!mounted) return;
      if (widget.account.owner == null) {
        Navigator.of(context).pop();
      } else {
        setState(() => _notice = error.message);
      }
    } finally {
      if (mounted) setState(() => _submitting = false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final listing = _listing;
    final path = listing?.breadcrumbs;
    final current = path?.last.id;
    final parent = path != null && path.length > 1
        ? path[path.length - 2]
        : null;
    final retry = _failedPage;
    final notice = _notice;
    final cursor = listing?.nextCursor;
    final ready =
        current != null &&
        current != widget.sourceParentId &&
        retry == null &&
        !_busy;
    return PopScope(
      canPop: !_submitting,
      child: Scaffold(
        appBar: AppBar(
          title: const Text('移动文件'),
          automaticallyImplyLeading: !_submitting,
        ),
        body: SafeArea(
          child: Column(
            children: [
              Expanded(
                child: ListView(
                  key: ValueKey(current),
                  padding: const EdgeInsets.symmetric(vertical: 4),
                  children: [
                    ListTile(
                      title: Text(
                        widget.file.name,
                        maxLines: 2,
                        overflow: TextOverflow.ellipsis,
                      ),
                      subtitle: path == null
                          ? null
                          : Text(
                              path
                                  .map(
                                    (entry) =>
                                        entry.name.isEmpty ? '根目录' : entry.name,
                                  )
                                  .join(' / '),
                            ),
                      trailing: parent == null
                          ? null
                          : IconButton(
                              tooltip: '上一级文件夹',
                              onPressed: _busy ? null : () => _open(parent.id),
                              icon: const Icon(Icons.arrow_upward),
                            ),
                    ),
                    if (_busy) const LinearProgressIndicator(),
                    if (notice != null)
                      Padding(
                        padding: const EdgeInsets.symmetric(
                          horizontal: 16,
                          vertical: 8,
                        ),
                        child: Semantics(liveRegion: true, child: Text(notice)),
                      ),
                    if (retry != null)
                      TextButton(
                        onPressed: () => _open(retry.$1, retry.$2),
                        child: const Text('重试读取'),
                      ),
                    if (listing != null) ...[
                      for (final folder in listing.entries)
                        ListTile(
                          leading: const Icon(Icons.folder_outlined),
                          title: Text(
                            folder.name,
                            maxLines: 2,
                            overflow: TextOverflow.ellipsis,
                          ),
                          trailing: const Icon(Icons.chevron_right),
                          onTap: _busy ? null : () => _open(folder.id),
                        ),
                      if (cursor != null)
                        TextButton(
                          onPressed: _busy
                              ? null
                              : () => _open(current!, cursor),
                          child: const Text('加载更多文件夹'),
                        ),
                      if (listing.entries.isEmpty &&
                          cursor == null &&
                          !_busy &&
                          retry == null)
                        const Center(child: Text('此处没有子文件夹')),
                    ],
                  ],
                ),
              ),
              Padding(
                padding: const EdgeInsets.fromLTRB(16, 4, 16, 8),
                child: FilledButton(
                  onPressed: ready ? () => _move(current) : null,
                  child: Text(_submitting ? '正在移动…' : '移动到此处'),
                ),
              ),
            ],
          ),
        ),
      ),
    );
  }
}
