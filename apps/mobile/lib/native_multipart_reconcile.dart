import 'dart:convert';
import 'dart:io';

import 'native_login.dart';
import 'native_multipart_upload.dart';
import 'native_upload_creation.dart';

Future<Map<int, MultipartReceipt>> reconcileMultipartReceipts(
  NativeSession session,
  MultipartCreation plan,
  List<String> sourceDigests,
) async {
  if (sourceDigests.length != plan.partCount) {
    throw const AccountFailure('源文件分片摘要与计划不一致。');
  }
  // 1. 读取原任务的清单。完整响应最多包含 10000 条记录。
  final reply = await requestAuthorizedJson(
    session.address,
    session.accessToken,
    '/api/v1/uploads/${plan.taskId}/parts',
    maxResponseBytes: 4 * 1024 * 1024,
  );
  if (reply
      case (
        status: HttpStatus.ok,
        body: {
          'data': {
            'part_size_bytes': final int partSize,
            'part_count': final int partCount,
            'parts': final List<Object?> parts,
          },
        },
      )
      when partSize == plan.partSizeBytes &&
          partCount == plan.partCount &&
          parts.length <= partCount) {
    // 2. 先核对清单结构，长度或内容与源文件不同的片留给补传。
    final confirmed = <int, MultipartReceipt>{};
    var previous = 0;
    for (final row in parts) {
      if (row
          case {
            'part_number': final int number,
            'size_bytes': final int size,
            'checksum_sha256': final String checksum,
            'etag': final String etag,
          }
          when number > previous &&
              number <= partCount &&
              size > 0 &&
              size <= maxUploadProtocolBytes &&
              isMultipartETag(etag)) {
        final sha = _parseChecksum(checksum);
        if (sha == null) {
          throw const AccountFailure('存储分片摘要无效，暂时无法核对。');
        }
        previous = number;
        if (size == plan.sizeOf(number) && sha == sourceDigests[number - 1]) {
          confirmed[number] = (
            number: number,
            sizeBytes: size,
            sha256: sha,
            etag: etag,
          );
        }
        continue;
      }
      throw const AccountFailure('存储分片清单无效，暂时无法核对。');
    }
    return confirmed;
  }
  throw const AccountFailure('无法核对存储分片清单，请恢复网络后重试。');
}

String? _parseChecksum(String value) {
  if (value.length != 44) return null;
  try {
    final bytes = base64Decode(value);
    if (bytes.length == 32 && base64Encode(bytes) == value) {
      return bytes.map((byte) => byte.toRadixString(16).padLeft(2, '0')).join();
    }
  } on FormatException {
    return null;
  }
  return null;
}
