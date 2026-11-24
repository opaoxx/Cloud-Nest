import 'dart:io';

import 'native_login.dart';

const uploadMebibyte = 1024 * 1024;
const maxSingleUploadBytes = 16 * uploadMebibyte;
const maxUploadProtocolBytes = 9007199254740991;

typedef PreparedUploadFile = ({String name, int sizeBytes, String sha256});

Map<String, Object> uploadDeclarationPayload(
  String parentId,
  PreparedUploadFile source,
) => {
  'parent_id': parentId,
  'name': source.name,
  'size_bytes': source.sizeBytes,
  'sha256': source.sha256,
};

Future<Object?> requestUploadCreation(
  NativeSession session,
  String key,
  String parentId,
  PreparedUploadFile source,
) async {
  // 同一个创建键始终对应同一份文件声明。
  final reply = await requestAuthorizedJson(
    session.address,
    session.accessToken,
    '/api/v1/uploads',
    payload: uploadDeclarationPayload(parentId, source),
    creationKey: key,
    successStatus: HttpStatus.created,
  );
  if (reply.status == HttpStatus.created) return reply.body;
  if (reply.status == HttpStatus.notFound) {
    throw const AccountFailure('目标目录不存在或已不可访问。');
  }
  if (reply.body case {'error': {'code': final String code}}) {
    switch (code) {
      case 'IDEMPOTENCY_EXPIRED':
        throw const AccountFailure('原创建回执已过期，请先核对目标目录。');
      case 'IDEMPOTENCY_CONFLICT':
        throw const AccountFailure('服务端记录的上传声明与本机不一致。');
      case 'FILE_TOO_LARGE':
        throw const AccountFailure('文件超过服务端允许上传的大小。');
    }
  }
  throw const AccountFailure('服务端未确认创建上传任务，可以重试同一文件和目录。');
}
