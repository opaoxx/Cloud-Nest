import 'dart:io';

import 'native_login.dart';

typedef UploadTaskTarget = ({String id, String mode, int sizeBytes});

typedef UploadTaskSnapshot = ({
  String state,
  String? entryId,
  String? errorCode,
  DateTime expiresAt,
});

final _errorCodePattern = RegExp(r'^[A-Z][A-Z_0-9]{0,63}$');
const _uploadStates = {
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

bool uploadIsTerminal(String state) =>
    const {'CANCELLED', 'COMPLETED', 'FAILED', 'EXPIRED'}.contains(state);

Future<UploadTaskSnapshot> readUploadTaskStatus(
  NativeSession session,
  UploadTaskTarget target,
  String parentId,
) async {
  final response = await requestAuthorizedJson(
    session.address,
    session.accessToken,
    '/api/v1/uploads/${target.id}',
  );
  if (response.status == HttpStatus.ok) {
    final task = parseUploadTaskSnapshot(response.body, target, parentId);
    if (task != null) return task;
  }
  throw const AccountFailure('无法核对原上传任务，请重试。');
}

Future<UploadTaskSnapshot> requestUploadCancellation(
  NativeSession session,
  UploadTaskTarget target,
  String parentId,
) async {
  // 202 只说明取消已受理。200 返回竞争后的实际终态。
  final reply = await requestAuthorizedJson(
    session.address,
    session.accessToken,
    '/api/v1/uploads/${target.id}/cancel',
    emptyPost: true,
    successStatus: HttpStatus.accepted,
    additionalJsonStatuses: const {HttpStatus.ok, HttpStatus.conflict},
  );
  final task = parseUploadTaskSnapshot(reply.body, target, parentId);
  if (task != null &&
      ((reply.status == HttpStatus.accepted && task.state == 'CANCELLING') ||
          (reply.status == HttpStatus.ok && uploadIsTerminal(task.state)))) {
    return task;
  }
  if (reply.status == HttpStatus.conflict) {
    throw const AccountFailure('服务端暂未受理取消，请查看原任务或重试。');
  }
  throw const AccountFailure('取消请求结果未确认，请查看原任务。');
}

UploadTaskSnapshot? parseUploadTaskSnapshot(
  Object? body,
  UploadTaskTarget target,
  String parentId,
) {
  if (body
      case {
        'data': {
          'id': final String id,
          'parent_id': final String actualParent,
          'size_bytes': final int size,
          'upload_mode': final String mode,
          'status': final String state,
          'expires_at': final String expiry,
          'completed_at': final String? completedAt,
          'result_entry_id': final String? entryId,
          'last_error': final Object? error,
        },
      }
      when id == target.id &&
          actualParent == parentId &&
          size == target.sizeBytes &&
          mode == target.mode &&
          _uploadStates.contains(state)) {
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
        ((state == 'COMPLETING' ||
                state == 'VERIFYING' ||
                state == 'CANCELLING') &&
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
