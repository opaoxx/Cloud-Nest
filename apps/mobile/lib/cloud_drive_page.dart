import 'dart:async';

import 'package:material_ui/material_ui.dart';

import 'mobile_account.dart';
import 'native_directory.dart';
import 'native_login.dart';
import 'native_upload_creation.dart';

class CloudDrivePage extends StatefulWidget {
  const CloudDrivePage({super.key, required this.account, this.source});

  final MobileAccount account;
  final PreparedUploadFile? source;

  @override
  State<CloudDrivePage> createState() => _CloudDrivePageState();
}

class _CloudDrivePageState extends State<CloudDrivePage> {
  String? _rootId;
  List<DirectoryBreadcrumb> _breadcrumbs = [];
  List<DirectoryEntry> _entries = [];
  String? _nextCursor;
  bool _loading = false;
  bool _transferBusy = false;
  String? _transferNotice;
  ({String message, String? parentId, String? cursor})? _failure;

  String? get _currentDirectory =>
      _breadcrumbs.isEmpty ? null : _breadcrumbs.last.id;

  @override
  void initState() {
    super.initState();
    unawaited(_load(null));
  }

  Future<void> _load(String? parentId, {String? cursor}) async {
    if (_loading || _transferBusy) return;
    setState(() {
      _loading = true;
      _failure = null;
      if (cursor == null && parentId != _currentDirectory) {
        _transferNotice = null;
      }
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

  Future<void> _confirmUpload() async {
    final source = widget.source;
    final target = _currentDirectory;
    if (source == null || target == null || _loading || _transferBusy) {
      return;
    }
    final multipart = source.sizeBytes > maxSingleUploadBytes;
    final path = _breadcrumbs
        .map((crumb) => crumb.name.isEmpty ? '根目录' : crumb.name)
        .join(' / ');
    final accepted = await showDialog<bool>(
      context: context,
      builder: (dialogContext) => AlertDialog(
        title: Text(multipart ? '在当前目录创建分片任务' : '上传到当前目录'),
        scrollable: true,
        content: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          mainAxisSize: MainAxisSize.min,
          children: [
            Text(source.name),
            Text('${source.sizeBytes} B'),
            Text(path),
          ],
        ),
        actions: [
          TextButton(
            onPressed: () => Navigator.of(dialogContext).pop(false),
            child: const Text('取消'),
          ),
          FilledButton(
            onPressed: () => Navigator.of(dialogContext).pop(true),
            child: Text(multipart ? '创建任务' : '上传'),
          ),
        ],
      ),
    );
    if (!mounted ||
        accepted != true ||
        _currentDirectory != target ||
        _loading ||
        _transferBusy) {
      return;
    }

    // 分片创建确认后显示任务计划，小文件发布成功后刷新目录。
    setState(() {
      _transferBusy = true;
      _transferNotice = multipart ? '正在创建分片任务…' : '正在上传并等待服务端校验…';
    });
    var published = false;
    try {
      if (multipart) {
        final creation = await widget.account.prepareMultipartUpload(
          source,
          target,
        );
        if (mounted) {
          setState(() => _transferNotice = '已创建分片任务。\n${creation.description}');
        }
      } else {
        await widget.account.uploadSingle(source, target);
        published = true;
        if (mounted) setState(() => _transferNotice = '上传成功。');
      }
    } on AccountFailure catch (error) {
      if (!mounted) return;
      if (widget.account.owner == null) {
        Navigator.of(context).pop(error.message);
      } else {
        setState(() => _transferNotice = error.message);
      }
    } finally {
      if (mounted) setState(() => _transferBusy = false);
    }
    if (published && mounted) await _load(target);
  }

  Future<void> _download(DirectoryEntry entry) async {
    if (_loading || _transferBusy) return;
    setState(() {
      _transferBusy = true;
      _transferNotice = '正在下载并校验文件…';
    });
    try {
      final name = await widget.account.downloadFile(entry);
      if (!mounted) return;
      setState(() => _transferNotice = name == null ? '已取消保存。' : '已保存「$name」。');
    } on AccountFailure catch (error) {
      if (!mounted) return;
      if (widget.account.owner == null) {
        Navigator.of(context).pop(error.message);
      } else {
        setState(() => _transferNotice = error.message);
      }
    } finally {
      if (mounted) setState(() => _transferBusy = false);
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
              onPressed: _loading || _transferBusy
                  ? null
                  : () => _load(_breadcrumbs[index].id),
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
          trailing: entry.isFolder
              ? const Icon(Icons.chevron_right)
              : IconButton(
                  tooltip: '下载 ${entry.name}',
                  onPressed: _loading || _transferBusy
                      ? null
                      : () => _download(entry),
                  icon: const Icon(Icons.download_outlined),
                ),
          onTap: entry.isFolder && !_loading && !_transferBusy
              ? () => _load(entry.id)
              : null,
        );
      },
    );
  }

  @override
  Widget build(BuildContext context) {
    final failure = _failure;
    final cursor = _nextCursor;
    final source = widget.source;
    final notice = _transferNotice;
    return PopScope(
      canPop: !_transferBusy,
      child: Scaffold(
        appBar: AppBar(
          title: const Text('云盘'),
          automaticallyImplyLeading: !_transferBusy,
          actions: [
            IconButton(
              tooltip: '刷新目录',
              onPressed: _loading || _transferBusy
                  ? null
                  : () => _load(_currentDirectory),
              icon: const Icon(Icons.refresh),
            ),
          ],
        ),
        body: SafeArea(
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.stretch,
            children: [
              if (_breadcrumbs.isNotEmpty) _pathBar(),
              if (_loading || _transferBusy) const LinearProgressIndicator(),
              if (notice != null)
                Padding(
                  padding: const EdgeInsets.symmetric(
                    horizontal: 16,
                    vertical: 8,
                  ),
                  child: Semantics(liveRegion: true, child: Text(notice)),
                ),
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
                    onPressed: _loading || _transferBusy
                        ? null
                        : () => _load(_currentDirectory, cursor: cursor),
                    child: const Text('加载更多'),
                  ),
                ),
              if (source != null && _currentDirectory != null)
                Padding(
                  padding: const EdgeInsets.fromLTRB(16, 8, 16, 12),
                  child: FilledButton(
                    onPressed: _loading || _transferBusy
                        ? null
                        : _confirmUpload,
                    child: Text(
                      source.sizeBytes > maxSingleUploadBytes
                          ? '在此目录创建分片任务'
                          : '上传到此目录',
                    ),
                  ),
                ),
            ],
          ),
        ),
      ),
    );
  }
}
