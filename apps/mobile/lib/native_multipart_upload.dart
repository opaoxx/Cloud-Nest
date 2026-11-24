import 'dart:convert';
import 'dart:io';
import 'dart:math';

import 'package:flutter/services.dart';

import 'native_login.dart';
import 'native_storage_url.dart';
import 'native_upload_creation.dart';

const _documents = MethodChannel('cloudnest/android_document');
const _checksumHeader = 'x-amz-checksum-sha256';
final _sha256Pattern = RegExp(r'^[0-9a-f]{64}$');
final _etagPattern = RegExp(r'^[A-Za-z0-9._:-]{1,64}$');

typedef MultipartPart = ({
  int number,
  int offsetBytes,
  int sizeBytes,
  String sha256,
});
typedef MultipartReceipt = ({
  int number,
  int sizeBytes,
  String sha256,
  String etag,
});
typedef MultipartPartGrant = ({
  String signedUrl,
  String checksum,
  DateTime expiresAt,
});

bool isMultipartSha256(String digest) =>
    _sha256Pattern.stringMatch(digest) == digest;
bool isMultipartETag(String etag) => _etagPattern.stringMatch(etag) == etag;

class MultipartCreation {
  const MultipartCreation._(
    this.taskId,
    this.sizeBytes,
    this.partSizeBytes,
    this.partCount,
    this.expiresAt,
  );

  static MultipartCreation? checked({
    required String taskId,
    required int sizeBytes,
    required int partSizeBytes,
    required int partCount,
    required String expiresAt,
  }) {
    // 按向上取整核对片数，末片可以短于计划片长。
    if (!isCloudNestId(taskId) ||
        sizeBytes <= maxSingleUploadBytes ||
        sizeBytes > maxUploadProtocolBytes ||
        partSizeBytes < maxSingleUploadBytes ||
        partSizeBytes > maxUploadProtocolBytes ||
        partSizeBytes % uploadMebibyte != 0 ||
        partCount < 1 ||
        partCount > 10000 ||
        (sizeBytes - 1) ~/ partSizeBytes + 1 != partCount ||
        DateTime.tryParse(expiresAt)?.isUtc != true) {
      return null;
    }
    return MultipartCreation._(
      taskId,
      sizeBytes,
      partSizeBytes,
      partCount,
      expiresAt,
    );
  }

  final String taskId;
  final int sizeBytes;
  final int partSizeBytes;
  final int partCount;
  final String expiresAt;

  String get description => '任务 $taskId · $partCount 片 · 片长 $partSizeBytes B';

  int offsetOf(int number) => (number - 1) * partSizeBytes;
  int sizeOf(int number) => min(partSizeBytes, sizeBytes - offsetOf(number));
}

Future<MultipartCreation> createMultipartUpload(
  NativeSession session,
  String key,
  String parentId,
  PreparedUploadFile source,
) async {
  // 1. 提交已落库的声明。幂等重放同样返回创建时的任务快照。
  final body = await requestUploadCreation(session, key, parentId, source);

  // 2. 核对目标、大小、模式和有效期，再验证分片计划。
  if (body
      case {
        'data': {
          'upload_mode': 'MULTIPART',
          'part_size_bytes': final int partSizeBytes,
          'part_count': final int partCount,
          'expires_at': final String expiresAt,
          'task': {
            'id': final String taskId,
            'parent_id': final String actualParent,
            'size_bytes': final int sizeBytes,
            'upload_mode': 'MULTIPART',
            'status': 'CREATING',
            'expires_at': final String taskExpiresAt,
          },
        },
      }
      when actualParent == parentId &&
          sizeBytes == source.sizeBytes &&
          expiresAt == taskExpiresAt) {
    final creation = MultipartCreation.checked(
      taskId: taskId,
      sizeBytes: sizeBytes,
      partSizeBytes: partSizeBytes,
      partCount: partCount,
      expiresAt: expiresAt,
    );
    if (creation != null) return creation;
  }
  throw const AccountFailure('分片创建回执与本机声明不一致，可以重试同一文件和目录。');
}

Future<List<String>> inspectMultipartSource(
  String scope,
  PreparedUploadFile source,
  MultipartCreation plan,
) async {
  final response = await _sourceOperation('inspectMultipart', scope, source, {
    'partSizeBytes': plan.partSizeBytes,
    'partCount': plan.partCount,
  });
  if (response case {'partDigests': final List<Object?> values}
      when values.length == plan.partCount &&
          values.every(
            (value) => value is String && isMultipartSha256(value),
          )) {
    return values.cast<String>();
  }
  throw const AccountFailure('无法核对分片摘要，请重新选择文件。');
}

Future<MultipartPartGrant> requestMultipartPart(
  NativeSession session,
  MultipartCreation plan,
  MultipartPart part,
) async {
  // 业务接口接收十六进制摘要，直传签名头使用同一摘要的 Base64 表示。
  final checksum = base64Encode(
    List<int>.generate(32, (index) {
      final start = index * 2;
      return int.parse(part.sha256.substring(start, start + 2), radix: 16);
    }),
  );
  final response = await requestAuthorizedJson(
    session.address,
    session.accessToken,
    '/api/v1/uploads/${plan.taskId}/part-urls',
    payload: {
      'parts': [
        {'part_number': part.number, 'sha256': part.sha256},
      ],
    },
  );
  if (response
      case (
        status: HttpStatus.ok,
        body: {
          'data': {
            'expires_at': final String taskExpiry,
            'parts': [
              {
                'part_number': final int number,
                'size_bytes': final int size,
                'url': final String raw,
                'required_headers': final Map<Object?, Object?> headers,
                'url_expires_at': final String urlExpiry,
              },
            ],
          },
        },
      )
      when number == part.number &&
          size == part.sizeBytes &&
          headers.length == 1 &&
          headers[_checksumHeader] == checksum) {
    final url = parseStorageUrl(raw, session.address);
    final expiresAt = DateTime.tryParse(urlExpiry);
    final taskExpiresAt = DateTime.tryParse(taskExpiry);
    if (url != null &&
        expiresAt != null &&
        taskExpiresAt != null &&
        expiresAt.isAfter(DateTime.now()) &&
        !expiresAt.isAfter(taskExpiresAt)) {
      if ((
            url.queryParametersAll['partNumber'],
            url.queryParametersAll['uploadId'],
          )
          case ([final partNumber], [final uploadId])
          when partNumber == part.number.toString() && uploadId.isNotEmpty) {
        return (signedUrl: raw, checksum: checksum, expiresAt: expiresAt);
      }
    }
  }
  throw const AccountFailure('服务端未交付与该分片匹配的有效签名。');
}

Future<MultipartReceipt> sendMultipartPart(
  String scope,
  PreparedUploadFile source,
  MultipartPart part,
  MultipartPartGrant grant,
) async {
  if (!grant.expiresAt.isAfter(DateTime.now())) {
    throw const AccountFailure('分片签名已到期，请重试。');
  }
  final response = await _sourceOperation('putMultipartPart', scope, source, {
    'offsetBytes': part.offsetBytes,
    'lengthBytes': part.sizeBytes,
    'url': grant.signedUrl,
    'checksum': grant.checksum,
  });
  if (response case {'etag': final String etag} when isMultipartETag(etag)) {
    return (
      number: part.number,
      sizeBytes: part.sizeBytes,
      sha256: part.sha256,
      etag: etag,
    );
  }
  throw const AccountFailure('存储没有返回有效的 ETag。');
}

Future<Object?> _sourceOperation(
  String operation,
  String scope,
  PreparedUploadFile source,
  Map<String, Object> arguments,
) async {
  try {
    return await _documents.invokeMethod<Object?>(operation, {
      'scope': scope,
      'name': source.name,
      'sizeBytes': source.sizeBytes,
      'sha256': source.sha256,
      ...arguments,
    });
  } on PlatformException catch (error) {
    throw AccountFailure(switch (error.code) {
      'document_source_changed' => '源文件已变化，请返回首页重新选择。',
      'document_access_lost' => '无法访问原文件，请返回首页重新选择。',
      'multipart_put_unconfirmed' => '存储端未确认分片，请重试。',
      _ => '无法读取源文件，请返回首页重新选择。',
    });
  } on MissingPluginException {
    throw const AccountFailure('当前设备无法读取分片正文。');
  }
}
