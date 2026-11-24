import 'dart:io';

import 'native_login.dart';
import 'native_multipart_upload.dart';
import 'native_upload_status.dart';

String multipartFailureLabel(Object? code) => switch (code) {
  'CHECKSUM_MISMATCH' => '文件内容校验失败',
  'BODY_SIZE_MISMATCH' => '文件大小校验失败',
  'TARGET_UNAVAILABLE' => '目标目录不可用',
  'UPLOAD_PARTS_MISMATCH' => '服务端未能确认收到的文件内容',
  _ => '上传失败',
};

Future<UploadTaskSnapshot> readMultipartTask(
  NativeSession session,
  MultipartCreation plan,
  String parentId,
) => readUploadTaskStatus(session, multipartTarget(plan), parentId);

UploadTaskTarget multipartTarget(MultipartCreation plan) =>
    (id: plan.taskId, mode: 'MULTIPART', sizeBytes: plan.sizeBytes);

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

Future<UploadTaskSnapshot> requestMultipartCompletion(
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
  final task = parseUploadTaskSnapshot(
    reply.body,
    multipartTarget(plan),
    parentId,
  );
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
    throw const AccountFailure('服务端未能确认收到的文件内容，文件未发布。');
  }
  throw const AccountFailure('完成请求结果未确认。重试时会查询原任务。');
}
