import 'dart:io';

import 'native_login.dart';
import 'native_upload_creation.dart';

typedef SingleUploadTask = ({String state, String? entryId, String? errorCode});

Future<String> createSingleUpload(
  NativeSession session,
  String key,
  String parentId,
  PreparedUploadFile source,
) async {
  // 1. 严格复用已经落盘的声明和创建键。
  final body = await requestUploadCreation(session, key, parentId, source);
  // 2. 回执必须属于本次目标和 SINGLE 模式，正文只发给当前服务的任务路径。
  if (body
      case {
        'data': {
          'upload_mode': 'SINGLE',
          'content_path': final String contentPath,
          'task': {
            'id': final String id,
            'parent_id': final String actualParent,
            'size_bytes': final int size,
            'upload_mode': 'SINGLE',
            'status': 'UPLOADING',
          },
        },
      }
      when isCloudNestId(id) &&
          actualParent == parentId &&
          size == source.sizeBytes &&
          contentPath == '/api/v1/uploads/$id/content') {
    return id;
  }
  throw const AccountFailure('上传创建回执与本机记录不一致。');
}

Future<void> sendSingleUploadBody(
  NativeSession session,
  String taskId,
  File file,
  int sizeBytes,
) async {
  final reply = await requestAuthorizedJson(
    session.address,
    session.accessToken,
    '/api/v1/uploads/$taskId/content',
    fileBody: (file: file, sizeBytes: sizeBytes),
    successStatus: HttpStatus.accepted,
  );
  if (reply
      case (
        status: HttpStatus.accepted,
        body: {
          'data': {'task_id': final String returnedId, 'status': 'VERIFYING'},
        },
      )
      when returnedId == taskId) {
    return;
  }
  throw const AccountFailure('正文接收结果未确认，重试会先查询原任务。');
}

Future<SingleUploadTask> readSingleUploadTask(
  NativeSession session,
  String taskId,
  String parentId,
  int sizeBytes,
) async {
  final reply = await requestAuthorizedJson(
    session.address,
    session.accessToken,
    '/api/v1/uploads/$taskId',
  );
  if (reply
      case (
        status: HttpStatus.ok,
        body: {
          'data': {
            'id': final String actualId,
            'parent_id': final String actualParent,
            'size_bytes': final int actualSize,
            'upload_mode': 'SINGLE',
            'status': final String state,
            'result_entry_id': final String? entryId,
            'last_error': final Object? error,
          },
        },
      )
      when actualId == taskId &&
          actualParent == parentId &&
          actualSize == sizeBytes) {
    switch (state) {
      case 'COMPLETED' when entryId != null && isCloudNestId(entryId):
        return (state: state, entryId: entryId, errorCode: null);
      case 'UPLOADING' ||
              'VERIFYING' ||
              'CANCELLING' ||
              'EXPIRING' ||
              'FAILED' ||
              'EXPIRED' ||
              'CANCELLED'
          when entryId == null:
        final code = switch (error) {
          {'code': final String code} => code,
          _ => null,
        };
        return (state: state, entryId: null, errorCode: code);
    }
  }
  throw const AccountFailure('暂时无法确认上传任务状态，重试会先查询原任务。');
}
