import 'dart:io';

import 'native_login.dart';
import 'native_multipart_upload.dart';

typedef MultipartTask = ({
  String state,
  String? entryId,
  String? errorCode,
  DateTime expiresAt,
});

final _errorCodePattern = RegExp(r'^[A-Z][A-Z_0-9]{0,63}$');
const _multipartStates = {
  'CREATING',
  'UPLOADING',
  'COMPLETING',
  'VERIFYING',
  'COMPLETED',
  'CANCELLING',
  'CANCELLED',
  'EXPIRING',
  'EXPIRED',
  'FAILED',
};

String multipartFailureLabel(Object? code) => switch (code) {
  'CHECKSUM_MISMATCH' => '整文件 SHA-256 校验失败',
  'BODY_SIZE_MISMATCH' => '整文件大小校验失败',
  'TARGET_UNAVAILABLE' => '目标目录不可用',
  'UPLOAD_PARTS_MISMATCH' => '服务端核对分片回执不一致',
  _ => '分片上传失败',
};

Future<MultipartTask> readMultipartTask(
  NativeSession session,
  MultipartCreation plan,
  String parentId,
) async {
  final response = await requestAuthorizedJson(
    session.address,
    session.accessToken,
    '/api/v1/uploads/${plan.taskId}',
  );
  if (response.status == HttpStatus.ok) {
    final task = _parseTask(response.body, plan, parentId);
    if (task != null) return task;
  }
  throw const AccountFailure('无法核对原分片任务，请重试。');
}

Future<bool> multipartReady(
  NativeSession session,
  MultipartCreation plan,
  String parentId,
) async {
  final task = await readMultipartTask(session, plan, parentId);
  if (!task.expiresAt.isAfter(DateTime.now()) ||
      task.state == 'EXPIRED' ||
      task.state == 'EXPIRING') {
    throw const AccountFailure('原分片任务的有效期已结束。');
  }
  if (task.state == 'UPLOADING') return true;
  if (task.state == 'CREATING') return false;
  throw const AccountFailure('原任务当前无法接收分片。');
}

Future<MultipartTask> requestMultipartCompletion(
  NativeSession session,
  MultipartCreation plan,
  String parentId,
  Map<int, MultipartReceipt> receipts,
) async {
  // 1. 只有完整且顺序正确的片号及 ETag 才能请求合并。
  if (receipts.length != plan.partCount) {
    throw const AccountFailure('分片回执不完整，请先继续上传。');
  }
  final parts = <Map<String, Object>>[];
  for (var number = 1; number <= plan.partCount; number++) {
    final receipt = receipts[number];
    if (receipt == null ||
        receipt.number != number ||
        receipt.sizeBytes != plan.sizeOf(number) ||
        !isMultipartETag(receipt.etag)) {
      throw const AccountFailure('分片回执与计划不一致。');
    }
    parts.add({'part_number': number, 'etag': receipt.etag});
  }

  // 2. 202 只确认已受理。200 只接受已经发布的同一任务。
  final reply = await requestAuthorizedJson(
    session.address,
    session.accessToken,
    '/api/v1/uploads/${plan.taskId}/complete',
    payload: {'parts': parts},
    successStatus: HttpStatus.accepted,
    additionalJsonStatuses: const {HttpStatus.ok, HttpStatus.conflict},
  );
  final task = _parseTask(reply.body, plan, parentId);
  if (task != null &&
      ((reply.status == HttpStatus.accepted &&
              (task.state == 'COMPLETING' || task.state == 'VERIFYING')) ||
          (reply.status == HttpStatus.ok && task.state == 'COMPLETED'))) {
    return task;
  }
  if ((reply.status, reply.body) case (
    HttpStatus.conflict,
    {'error': {'code': 'UPLOAD_PARTS_MISMATCH'}},
  )) {
    throw const AccountFailure('服务端核对分片回执不一致，文件未发布。');
  }
  throw const AccountFailure('完成请求结果未确认。重试时会查询原任务。');
}

MultipartTask? _parseTask(
  Object? body,
  MultipartCreation plan,
  String parentId,
) {
  if (body
      case {
        'data': {
          'id': final String id,
          'parent_id': final String actualParent,
          'size_bytes': final int size,
          'upload_mode': 'MULTIPART',
          'status': final String state,
          'expires_at': final String expiry,
          'completed_at': final String? completedAt,
          'result_entry_id': final String? entryId,
          'last_error': final Object? error,
        },
      }
      when id == plan.taskId &&
          actualParent == parentId &&
          size == plan.sizeBytes &&
          _multipartStates.contains(state)) {
    final expiresAt = DateTime.tryParse(expiry);
    if (expiresAt == null) return null;
    if (state == 'COMPLETED') {
      if (entryId == null ||
          !isCloudNestId(entryId) ||
          completedAt == null ||
          DateTime.tryParse(completedAt) == null) {
        return null;
      }
    } else if (entryId != null ||
        ((state == 'COMPLETING' || state == 'VERIFYING') &&
            completedAt != null)) {
      return null;
    }
    final code = switch (error) {
      {'code': final String code}
          when _errorCodePattern.stringMatch(code) == code =>
        code,
      _ => null,
    };
    return (
      state: state,
      entryId: entryId,
      errorCode: code,
      expiresAt: expiresAt,
    );
  }
  return null;
}
