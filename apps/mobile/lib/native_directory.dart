import 'dart:convert';
import 'dart:io';
import 'dart:math';

import 'native_login.dart';

typedef DirectoryEntry = ({
  String id,
  String name,
  bool isFolder,
  int sizeBytes,
  String revision,
});
typedef DirectoryBreadcrumb = ({String id, String name});
typedef DirectoryListing = ({
  List<DirectoryBreadcrumb> breadcrumbs,
  List<DirectoryEntry> entries,
  String? nextCursor,
});

const _maxCursorBytes = 1024;
const _maxJsonSafeInteger = 9007199254740991;

String newFileActionKey() {
  final random = Random.secure();
  return List.generate(
    16,
    (_) => random.nextInt(256).toRadixString(16).padLeft(2, '0'),
  ).join();
}

Future<DirectoryEntry> changeDirectoryFile(
  NativeSession session,
  DirectoryEntry entry,
  String parentId,
  String key, {
  String? name,
}) async {
  // 1. 用选中项的 ID、当前父目录和所见修订提交一次文件操作。
  if (entry.isFolder ||
      !isCloudNestId(entry.id) ||
      !isCloudNestId(entry.revision) ||
      !isCloudNestId(parentId)) {
    throw const AccountFailure('文件信息有误，请刷新目录后重试。');
  }
  final path = '/api/v1/entries/${entry.id}';
  final reply = await requestAuthorizedJson(
    session.address,
    session.accessToken,
    name == null ? '$path/trash' : path,
    method: name == null ? 'POST' : 'PATCH',
    payload: {'expected_revision': entry.revision, 'name': ?name},
    creationKey: key,
    additionalJsonStatuses: const {HttpStatus.badRequest},
  );
  // 2. 只接受同一文件与目录的成功快照。
  if (reply case (
    status: HttpStatus.ok,
    body: {'data': {'entry': final Map<String, Object?> snapshot}},
  )) {
    // 重命名返回文件详情，回收返回带删除时间的简要快照。
    final matchesAction = name == null
        ? snapshot['entry_type'] == 'FILE' && snapshot['deleted_at'] is String
        : snapshot['kind'] is String &&
              snapshot['kind'] != 'folder' &&
              snapshot['size_bytes'] == entry.sizeBytes;
    if (snapshot
        case {
          'id': final String id,
          'parent_id': final String actualParent,
          'name': final String actualName,
          'revision': final String revision,
          'status': final String status,
        }
        when matchesAction &&
            id == entry.id &&
            actualParent == parentId &&
            actualName.isNotEmpty &&
            isCloudNestId(revision) &&
            status == (name == null ? 'TRASHED' : 'ACTIVE')) {
      return (
        id: id,
        name: actualName,
        isFolder: false,
        sizeBytes: entry.sizeBytes,
        revision: revision,
      );
    }
  }

  // 3. 显示明确的冲突原因，保留目录项供用户刷新或修正。
  if (reply.status == HttpStatus.notFound) {
    throw const AccountFailure('文件不存在或已不可访问，请刷新目录。');
  }
  final code = switch (reply.body) {
    {'error': {'code': final String value}} => value,
    _ => '',
  };
  throw AccountFailure(switch (code) {
    'NAME_CONFLICT' => '当前目录已有同名文件，请换一个名称。',
    'INVALID_NAME' => '文件名不符合服务端规则，请换一个名称。',
    'REVISION_CONFLICT' => '文件已发生变化，请刷新目录后重试。',
    _ => '服务端未确认文件操作，请刷新目录核对结果。',
  });
}

Future<DirectoryListing> readDirectoryListing(
  NativeSession session, {
  required String rootId,
  required String parentId,
  String? cursor,
}) async {
  // 1. 每页使用同一个父目录 ID。游标只作为不透明文本放进查询。
  if (!isCloudNestId(rootId) || !isCloudNestId(parentId)) {
    throw const AccountFailure('目录 ID 无效，请返回云盘后重试。');
  }
  if (cursor != null && !_validCursor(cursor)) {
    throw const AccountFailure('续页游标已失效，请刷新目录。');
  }
  final reply = await requestAuthorizedJson(
    session.address,
    session.accessToken,
    '/api/v1/entries',
    query: {'parent_id': parentId, 'limit': '20', 'cursor': ?cursor},
    maxResponseBytes: 256 * 1024,
  );
  switch (reply.status) {
    case HttpStatus.ok:
      break;
    case HttpStatus.notFound:
      throw const AccountFailure('目录不存在或已不可访问。');
    case HttpStatus.badRequest when cursor != null:
      throw const AccountFailure('续页游标已失效，请刷新目录。');
    default:
      throw const AccountFailure('无法读取云盘目录，请稍后重试。');
  }
  if (reply.body
      case {
        'data': {
          'items': final List<Object?> items,
          'breadcrumbs': final List<Object?> path,
          'next_cursor': final String? nextCursor,
        },
      }
      when items.length <= 100 && path.isNotEmpty && path.length <= 33) {
    // 2. 按服务端返回的真实路径导航，校验根、终点及子项父目录。
    final breadcrumbs = _readBreadcrumbs(path, rootId, parentId);
    final entries = items.map((item) => _readEntry(item, parentId)).toList();

    // 3. 末页用 null 表示。有续页时，游标必须能向前推进。
    if (nextCursor != null &&
        (!_validCursor(nextCursor) ||
            nextCursor == cursor ||
            entries.isEmpty)) {
      throw const AccountFailure('服务返回的续页游标有误，请刷新目录。');
    }
    return (breadcrumbs: breadcrumbs, entries: entries, nextCursor: nextCursor);
  }
  throw const AccountFailure('服务返回的目录页格式有误。');
}

List<DirectoryBreadcrumb> _readBreadcrumbs(
  List<Object?> path,
  String rootId,
  String parentId,
) {
  const failure = AccountFailure('服务返回的目录路径与请求不一致。');
  final breadcrumbs = <DirectoryBreadcrumb>[];
  final seen = <String>{};
  for (final item in path) {
    if (item case {'id': final String id, 'name': final String name}
        when isCloudNestId(id) &&
            name.isEmpty == breadcrumbs.isEmpty &&
            seen.add(id)) {
      breadcrumbs.add((id: id, name: name));
    } else {
      throw failure;
    }
  }
  if (breadcrumbs.first.id != rootId || breadcrumbs.last.id != parentId) {
    throw failure;
  }
  return breadcrumbs;
}

DirectoryEntry _readEntry(Object? item, String parentId) {
  if (item
      case {
        'id': final String id,
        'parent_id': final String actualParentId,
        'name': final String name,
        'entry_type': final String type,
        'size_bytes': final int sizeBytes,
        'revision': final String revision,
      }
      when isCloudNestId(id) &&
          isCloudNestId(revision) &&
          actualParentId == parentId &&
          name.isNotEmpty &&
          (type == 'DIR' || type == 'FILE') &&
          sizeBytes >= 0 &&
          sizeBytes <= _maxJsonSafeInteger) {
    return (
      id: id,
      name: name,
      isFolder: type == 'DIR',
      sizeBytes: sizeBytes,
      revision: revision,
    );
  }
  throw const AccountFailure('服务返回的目录项与请求不一致。');
}

bool _validCursor(String cursor) =>
    cursor.isNotEmpty && utf8.encode(cursor).length <= _maxCursorBytes;
