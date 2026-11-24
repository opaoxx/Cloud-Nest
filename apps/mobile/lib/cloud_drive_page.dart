import 'dart:async';

import 'package:material_ui/material_ui.dart';

import 'file_name_dialog.dart';
import 'mobile_account.dart';
import 'native_directory.dart';
import 'native_login.dart';
import 'native_upload_creation.dart';
import 'transfer_pause.dart';

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
  TransferPause? _pause;
  String? _transferNotice;
  String? _pendingUploadName;
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
    // 1. 确认云盘名称和目标目录，保留所选文件的正文身份。
    final name = await showFileNameDialog(
      context,
      title: '上传到当前目录',
      initialName: _pendingUploadName ?? source.name,
      actionLabel: '上传',
      details: [Text(path), Text('${source.sizeBytes} B')],
    );
    if (!mounted ||
        name == null ||
        _currentDirectory != target ||
        _loading ||
        _transferBusy) {
      return;
    }

    // 2. 用同一发布名称创建或恢复任务，并按本机回执更新进度。
    final pause = multipart ? TransferPause() : null;
    setState(() {
      _transferBusy = true;
      _pause = pause;
      _pendingUploadName = name;
      _transferNotice = multipart ? '正在核对分片任务…' : '正在上传并等待服务端校验…';
    });
    var published = false;
    try {
      if (multipart) {
        await widget.account.uploadMultipart(
          source,
          target,
          uploadName: name,
          pause: pause,
          onProgress: (plan, confirmed) {
            if (!mounted) return;
            final progress = '已确认 $confirmed/${plan.partCount} 片';
            final status = confirmed == plan.partCount
                ? '$progress，等待校验和发布…'
                : progress;
            setState(() => _transferNotice = '$status\n${plan.description}');
          },
        );
      } else {
        await widget.account.uploadSingle(source, target, uploadName: name);
      }
      published = true;
      if (mounted) {
        setState(() {
          _pendingUploadName = null;
          _transferNotice = '上传成功。';
        });
      }
    } on AccountFailure catch (error) {
      if (!mounted) return;
      if (widget.account.owner == null) {
        Navigator.of(context).pop(error.message);
      } else {
        setState(() => _transferNotice = error.message);
      }
    } finally {
      if (mounted) {
        setState(() {
          _transferBusy = false;
          _pause = null;
        });
      }
    }
    // 3. 发布后按原目录 ID 读取服务端实际文件名。
    if (published && mounted) await _load(target);
  }

  Future<void> _download(DirectoryEntry entry) async {
    if (_loading || _transferBusy) return;
    final pause = TransferPause();
    setState(() {
      _transferBusy = true;
      _pause = pause;
      _transferNotice = '正在下载并校验文件…';
    });
    try {
      final name = await widget.account.downloadFile(entry, pause: pause);
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
      if (mounted) {
        setState(() {
          _transferBusy = false;
          _pause = null;
        });
      }
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

  Widget _entrySliver() {
    if (_entries.isEmpty) {
      final message = _breadcrumbs.isEmpty
          ? (_loading ? '正在读取云盘目录…' : null)
          : '此目录为空';
      final empty = message == null
          ? const SizedBox.shrink()
          : Center(child: Text(message));
      return _nextCursor == null
          ? SliverFillRemaining(hasScrollBody: false, child: empty)
          : SliverToBoxAdapter(child: empty);
    }
    return SliverList.builder(
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

  Widget _directoryHeader() {
    final notice = _transferNotice;
    final failure = _failure;
    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: [
        if (_breadcrumbs.isNotEmpty) _pathBar(),
        if (_loading || _transferBusy) const LinearProgressIndicator(),
        if (notice != null)
          Padding(
            padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 8),
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
      ],
    );
  }

  @override
  Widget build(BuildContext context) {
    final cursor = _nextCursor;
    final source = widget.source;
    final pause = _pause;
    return PopScope(
      canPop: !_transferBusy,
      child: Scaffold(
        appBar: AppBar(
          title: const Text('云盘'),
          automaticallyImplyLeading: !_transferBusy,
          actions: [
            if (pause != null)
              TextButton(
                onPressed: pause.requested
                    ? null
                    : () => setState(pause.request),
                child: Text(pause.requested ? '正在暂停…' : '暂停'),
              ),
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
              // 1. 使用当前目录 ID，窗口变化时沿用页面和目录对应的滚动区域。
              Expanded(
                child: CustomScrollView(
                  key: ValueKey(_currentDirectory),
                  slivers: [
                    // 2. 路径、状态和文件列表共享纵向滚动区域，短窗口也能查看。
                    SliverToBoxAdapter(child: _directoryHeader()),
                    _entrySliver(),
                    if (cursor != null)
                      SliverPadding(
                        padding: const EdgeInsets.fromLTRB(16, 8, 16, 12),
                        sliver: SliverToBoxAdapter(
                          child: OutlinedButton(
                            onPressed: _loading || _transferBusy
                                ? null
                                : () =>
                                      _load(_currentDirectory, cursor: cursor),
                            child: const Text('加载更多'),
                          ),
                        ),
                      ),
                  ],
                ),
              ),
              // 3. 上传入口继续提交已选文件到当前目录，完成后按同一 ID 刷新。
              if (source != null && _currentDirectory != null)
                Padding(
                  padding: const EdgeInsets.fromLTRB(16, 8, 16, 12),
                  child: FilledButton(
                    onPressed: _loading || _transferBusy
                        ? null
                        : _confirmUpload,
                    child: const Text('上传到此目录'),
                  ),
                ),
            ],
          ),
        ),
      ),
    );
  }
}
