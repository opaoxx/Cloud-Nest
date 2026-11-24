import 'dart:convert';
import 'dart:io';

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
