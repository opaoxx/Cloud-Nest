import 'dart:io';

import 'native_login.dart';
import 'native_upload_creation.dart';

typedef UploadReuseReceipt = ({String entryId, String name});

Future<UploadReuseReceipt?> reuseUploadContent(
  NativeSession session,
  String key,
  String parentId,
  PreparedUploadFile source,
) async {
  // 1. 把原声明和同一个创建键交给当前账号的内容复用接口。
  final reply = await requestAuthorizedJson(
    session.address,
    session.accessToken,
    '/api/v1/files/reuse',
    payload: uploadDeclarationPayload(parentId, source),
    creationKey: key,
    successStatus: HttpStatus.created,
    additionalJsonStatuses: const {
      HttpStatus.notFound,
      HttpStatus.insufficientStorage,
    },
  );

  // 2. 只有明确的未命中才允许普通上传。
  if ((reply.status, reply.body) case (
    HttpStatus.notFound,
    {'error': {'code': 'REUSABLE_CONTENT_NOT_FOUND'}},
  )) {
    return null;
  }
  if (reply.status != HttpStatus.created) {
    final message = switch (reply.body) {
      {'error': {'code': 'ENTRY_NOT_FOUND'}} => '复用目标目录不存在或已不可访问。',
      {'error': {'code': 'QUOTA_EXCEEDED'}} => '账号剩余配额不足，无法发布该文件。',
      {'error': {'code': 'IDEMPOTENCY_EXPIRED'}} => '原复用回执已过期，请先核对目标目录。',
      {'error': {'code': 'IDEMPOTENCY_CONFLICT'}} => '原复用声明与本机记录不一致。',
      _ => '服务端未确认文件复用，请重试同一文件和目录。',
    };
    throw AccountFailure(message);
  }

  // 3. 核对新目录项，并保留服务端实际分配的文件名。
  if (reply.body
      case {
        'data': {
          'entry': {
            'id': final String entryId,
            'parent_id': final String actualParent,
            'name': final String name,
            'entry_type': 'FILE',
            'size_bytes': final int size,
            'revision': '1',
            'status': 'ACTIVE',
          },
        },
      }
      when isCloudNestId(entryId) &&
          actualParent == parentId &&
          name.trim().isNotEmpty &&
          size == source.sizeBytes) {
    return (entryId: entryId, name: name);
  }
  throw const AccountFailure('文件复用回执与本机声明不一致，请重试同一任务。');
}
