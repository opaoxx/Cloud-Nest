import 'dart:async';

import 'package:material_ui/material_ui.dart';

import 'file_name_dialog.dart';
import 'file_presentation.dart';
import 'mobile_account.dart';
import 'mobile_tasks_page.dart';
import 'native_directory.dart';
import 'native_document_selection.dart';
import 'native_login.dart';
import 'native_upload_creation.dart';
import 'transfer_pause.dart';

class CloudDrivePage extends StatefulWidget {
  const CloudDrivePage({
    super.key,
    required this.account,
    this.accountBusy = false,
    this.onLogout,
    this.onSessionEnded,
  });

  final MobileAccount account;
  final bool accountBusy;
  final VoidCallback? onLogout;
  final ValueChanged<String>? onSessionEnded;

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
  bool _choosingUpload = false;
  TransferPause? _pause;
  String? _transferNotice;
  ({PreparedUploadFile source, String target, String name})? _pendingUpload;
  ({String message, String? parentId, String? cursor})? _failure;

  String? get _currentDirectory =>
      _breadcrumbs.isEmpty ? null : _breadcrumbs.last.id;

  bool get _blocked =>
      _loading || _transferBusy || _choosingUpload || widget.accountBusy;

  @override
  void initState() {
    super.initState();
    unawaited(_load(null));
  }

  Future<void> _load(String? parentId, {String? cursor}) async {
    if (_loading || _transferBusy || _choosingUpload) return;
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
        if (_pendingUpload?.target != _currentDirectory) _pendingUpload = null;
      });
    } on AccountFailure catch (error) {
      // 3. 保留上次成功的列表。远端会话结束时回首页显示重新登录提示。
      if (!mounted || ModalRoute.of(context)?.isCurrent != true) return;
      if (widget.account.owner == null) {
        widget.onSessionEnded?.call(error.message);
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

  Future<void> _openTaskRecords() async {
    final owner = widget.account.owner;
    if (owner == null || _blocked) return;
    final notice = await Navigator.of(context).push<String>(
      MaterialPageRoute(
        builder: (context) =>
            MobileTasksPage(owner: owner, account: widget.account),
      ),
    );
    if (!mounted) return;
    if (widget.account.owner != owner) {
      widget.onSessionEnded?.call(notice ?? '会话已失效，请重新登录。');
      return;
    }
    // 任务记录中也可完成上传，返回时刷新当前目录。
    await _load(_currentDirectory);
  }

  Future<void> _chooseUpload() async {
    final target = _currentDirectory;
    final owner = widget.account.owner;
    if (target == null || owner == null || _blocked) return;
    final path = _breadcrumbs
        .map((crumb) => crumb.name.isEmpty ? '根目录' : crumb.name)
        .join(' / ');
    setState(() {
      _choosingUpload = true;
      _pendingUpload = null;
      _transferNotice = '正在读取所选文件…';
    });
    try {
      // 1. 在原位置打开系统选择器，记住发起时的目标目录。
      final source = await pickUploadFile(owner);
      if (!mounted || widget.account.owner != owner) return;
      setState(() => _transferNotice = null);
      if (source == null) return;

      // 2. 默认保留原文件名，可选改名，按一次上传即提交。
      final name = await showFileNameDialog(
        context,
        title: '上传到当前目录',
        initialName: source.name,
        actionLabel: '上传',
        autofocus: false,
        details: [Text(path), Text(fileSizeLabel(source.sizeBytes))],
      );
      if (!mounted || widget.account.owner != owner || name == null) return;
      _pendingUpload = (source: source, target: target, name: name);
    } on AccountFailure catch (error) {
      if (mounted) setState(() => _transferNotice = error.message);
    } finally {
      if (mounted) setState(() => _choosingUpload = false);
    }
    if (mounted && _pendingUpload != null) await _upload();
  }

  Future<void> _upload() async {
    final pending = _pendingUpload;
    if (pending == null || _blocked || pending.target != _currentDirectory) {
      return;
    }
    final (:source, :target, :name) = pending;
    final multipart = source.sizeBytes > maxSingleUploadBytes;
    final pause = multipart ? TransferPause() : null;
    // 3. 沿用已选名称和原目标开始上传，失败时可在本页重试。
    setState(() {
      _transferBusy = true;
      _pause = pause;
      _transferNotice = multipart ? '正在准备上传…' : '正在上传文件…';
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
            setState(() {
              _transferNotice = uploadProgressLabel(confirmed, plan.partCount);
            });
          },
        );
      } else {
        await widget.account.uploadSingle(source, target, uploadName: name);
      }
      published = true;
      if (mounted) {
        setState(() {
          _pendingUpload = null;
          _transferNotice = '上传成功。';
        });
      }
    } on AccountFailure catch (error) {
      if (!mounted) return;
      if (widget.account.owner == null) {
        widget.onSessionEnded?.call(error.message);
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
    if (_blocked) return;
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
        widget.onSessionEnded?.call(error.message);
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

  Future<void> _editFile(DirectoryEntry entry, {required bool rename}) async {
    final directory = _currentDirectory;
    if (directory == null || _blocked) return;

    // 1. 等用户提交新名称或明确确认移入回收站。
    String? name;
    if (rename) {
      name = await showFileNameDialog(
        context,
        title: '重命名文件',
        initialName: entry.name,
        actionLabel: '保存名称',
      );
      if (name == null || name == entry.name) return;
    } else {
      final confirmed = await showDialog<bool>(
        context: context,
        builder: (dialogContext) => AlertDialog(
          title: const Text('移入回收站'),
          content: Text('将「${entry.name}」移入回收站？'),
          actions: [
            TextButton(
              onPressed: () => Navigator.of(dialogContext).pop(),
              child: const Text('取消'),
            ),
            FilledButton(
              onPressed: () => Navigator.of(dialogContext).pop(true),
              child: const Text('移入回收站'),
            ),
          ],
        ),
      );
      if (confirmed != true) return;
    }
    if (!mounted || _currentDirectory != directory || _blocked) {
      return;
    }

    // 2. 发出原文件修订上的操作，等待服务端确认。
    setState(() {
      _transferBusy = true;
      _transferNotice = rename ? '正在重命名…' : '正在移入回收站…';
    });
    try {
      final updated = await widget.account.editFile(
        entry,
        directory,
        name: name,
      );
      if (!mounted) return;
      setState(() {
        if (rename) {
          _entries = [
            for (final listed in _entries)
              if (listed.id == updated.id) updated else listed,
          ];
        } else {
          _entries.removeWhere((listed) => listed.id == updated.id);
        }
        _transferNotice = rename ? '已重命名为「${updated.name}」。' : '已移入回收站。';
      });
    } on AccountFailure catch (error) {
      if (!mounted) return;
      if (widget.account.owner == null) {
        widget.onSessionEnded?.call(error.message);
      } else {
        setState(() => _transferNotice = error.message);
      }
      return;
    } finally {
      if (mounted) setState(() => _transferBusy = false);
    }
    // 3. 当前页先反映已确认快照，再读取服务端的新排序和分页。
    if (mounted) await _load(directory);
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
              onPressed: _blocked ? null : () => _load(_breadcrumbs[index].id),
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
          subtitle: entry.isFolder
              ? null
              : Text(fileSizeLabel(entry.sizeBytes)),
          trailing: entry.isFolder
              ? const Icon(Icons.chevron_right)
              : Row(
                  mainAxisSize: MainAxisSize.min,
                  children: [
                    IconButton(
                      tooltip: '下载 ${entry.name}',
                      onPressed: _blocked ? null : () => _download(entry),
                      icon: const Icon(Icons.download_outlined),
                    ),
                    IconButton(
                      tooltip: '重命名 ${entry.name}',
                      onPressed: _blocked
                          ? null
                          : () => _editFile(entry, rename: true),
                      icon: const Icon(Icons.edit_outlined),
                    ),
                    IconButton(
                      tooltip: '删除 ${entry.name}',
                      onPressed: _blocked
                          ? null
                          : () => _editFile(entry, rename: false),
                      icon: const Icon(Icons.delete_outline),
                    ),
                  ],
                ),
          onTap: entry.isFolder && !_blocked ? () => _load(entry.id) : null,
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
        if (_loading || _transferBusy || (_choosingUpload && notice != null))
          const LinearProgressIndicator(),
        if (notice != null)
          Padding(
            padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 8),
            child: Semantics(liveRegion: true, child: Text(notice)),
          ),
        if (_pendingUpload != null && !_blocked)
          Align(
            alignment: Alignment.centerLeft,
            child: TextButton(onPressed: _upload, child: const Text('重试上传')),
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
    final pause = _pause;
    final canGoUp = _breadcrumbs.length > 1;
    return PopScope(
      canPop: !_blocked && !canGoUp,
      onPopInvokedWithResult: (didPop, _) {
        if (!didPop && canGoUp && !_blocked) {
          _load(_breadcrumbs[_breadcrumbs.length - 2].id);
        }
      },
      child: Scaffold(
        appBar: AppBar(
          title: const Text('云盘'),
          automaticallyImplyLeading: false,
          leading: canGoUp
              ? IconButton(
                  tooltip: MaterialLocalizations.of(context).backButtonTooltip,
                  icon: const BackButtonIcon(),
                  onPressed: _blocked
                      ? null
                      : () => _load(_breadcrumbs[_breadcrumbs.length - 2].id),
                )
              : null,
          actions: [
            if (pause != null)
              TextButton(
                onPressed: pause.requested
                    ? null
                    : () => setState(pause.request),
                child: Text(pause.requested ? '正在暂停…' : '暂停'),
              ),
            IconButton(
              tooltip: '任务记录',
              onPressed: _blocked ? null : _openTaskRecords,
              icon: const Icon(Icons.history),
            ),
            PopupMenuButton<String>(
              tooltip: '云盘选项',
              enabled: !_blocked,
              onSelected: (action) {
                if (action == 'refresh') _load(_currentDirectory);
                if (action == 'logout') widget.onLogout?.call();
              },
              itemBuilder: (context) => [
                const PopupMenuItem(value: 'refresh', child: Text('刷新目录')),
                if (widget.onLogout != null)
                  const PopupMenuItem(value: 'logout', child: Text('退出登录')),
              ],
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
                            onPressed: _blocked
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
              // 3. 按钮固定在安全区内，切换目录时保留同一操作位置。
              Padding(
                padding: const EdgeInsets.fromLTRB(16, 4, 16, 8),
                child: Align(
                  alignment: Alignment.center,
                  child: FilledButton.icon(
                    key: const ValueKey('choose-upload'),
                    onPressed: _blocked || _currentDirectory == null
                        ? null
                        : _chooseUpload,
                    icon: const Icon(Icons.upload_file_outlined),
                    label: const Text('选择文件'),
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
