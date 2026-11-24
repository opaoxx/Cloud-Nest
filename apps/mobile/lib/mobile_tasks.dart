import 'package:flutter/services.dart';

import 'account_owner.dart';
import 'native_download.dart';
import 'native_login.dart';
import 'native_multipart_completion.dart';
import 'native_multipart_upload.dart';
import 'native_upload_creation.dart';

const _tasks = MethodChannel('cloudnest/android_tasks');

class TaskStoreFailure implements Exception {
  const TaskStoreFailure();
}

class MobileTask {
  const MobileTask(
    this.id,
    this.name,
    this.createdAt,
    this.statusLabel,
    this.canResume,
  );

  factory MobileTask.fromAndroid(Object? row) {
    if (row is! Map<Object?, Object?>) throw const TaskStoreFailure();
    if (row
        case {
          'id': final String id,
          'name': final String name,
          'kind': final String kind,
          'state': final String state,
          'createdAtMs': final int createdAtMs,
        }
        when id.isNotEmpty &&
            name.isNotEmpty &&
            createdAtMs >= 0 &&
            createdAtMs <= 8640000000000000) {
      final label = switch ((kind, state)) {
        ('TEST', 'TEST_RECORD') => '未执行',
        ('UPLOAD', 'PENDING') when row['partCount'] != null => _multipartStatus(
          row,
        ),
        ('UPLOAD', 'PENDING') => '待确认',
        ('UPLOAD', 'COMPLETED') => '已上传',
        ('UPLOAD', 'FAILED') when row['partCount'] != null =>
          multipartFailureLabel(row['errorCode']),
        ('UPLOAD', 'FAILED') => '失败',
        ('UPLOAD', 'EXPIRED') => '已过期',
        ('UPLOAD', 'CANCELLED') => '已取消',
        ('DOWNLOAD', 'COMPLETED') => '已下载',
        _ => throw const TaskStoreFailure(),
      };
      return MobileTask(
        id,
        name,
        DateTime.fromMillisecondsSinceEpoch(createdAtMs),
        label,
        kind == 'UPLOAD' && state == 'PENDING' && _isMultipartRow(row),
      );
    }
    throw const TaskStoreFailure();
  }

  final String id;
  final String name;
  final DateTime createdAt;
  final String statusLabel;
  final bool canResume;
}

bool _isMultipartRow(Map<Object?, Object?> row) => switch (row['sizeBytes']) {
  final int size
      when size > maxSingleUploadBytes && size <= maxUploadProtocolBytes =>
    true,
  _ => false,
};

typedef PendingMultipartUpload = ({
  String id,
  String parentId,
  PreparedUploadFile source,
  MultipartCreation? creation,
});

Future<PendingMultipartUpload?> readPendingMultipartUpload(
  AccountOwner owner,
  String id,
) async {
  final value = await _taskCall<Object?>('pendingMultipart', owner, {'id': id});
  if (value == null) return null;
  if (value
      case {
        'id': final String actualId,
        'parentId': final String parentId,
        'name': final String name,
        'sizeBytes': final int size,
        'sha256': final String sha256,
        'remoteId': final String? remoteId,
        'kind': 'UPLOAD',
        'state': 'PENDING',
      }
      when actualId == id &&
          isCloudNestId(parentId) &&
          name.isNotEmpty &&
          size > maxSingleUploadBytes &&
          size <= maxUploadProtocolBytes &&
          isMultipartSha256(sha256)) {
    return (
      id: id,
      parentId: parentId,
      source: (name: name, sizeBytes: size, sha256: sha256),
      creation: remoteId == null ? null : _savedMultipart(value, size),
    );
  }
  throw const TaskStoreFailure();
}

typedef LocalSingleIntent = ({String id, String? remoteId});

Future<LocalSingleIntent> reserveSingleIntent(
  AccountOwner owner,
  String parentId,
  PreparedUploadFile source,
) async {
  final value = await _reserveUpload(owner, parentId, source);
  if (value case {'id': final String id, 'remoteId': final String? remoteId}
      when id.isNotEmpty && (remoteId == null || isCloudNestId(remoteId))) {
    return (id: id, remoteId: remoteId);
  }
  throw const TaskStoreFailure();
}

Future<({String id, MultipartCreation? creation})> reserveMultipartIntent(
  AccountOwner owner,
  String parentId,
  PreparedUploadFile source,
) async {
  final value = await _reserveUpload(owner, parentId, source);
  if (value case {'id': final String id, 'remoteId': final String? remoteId}
      when id.isNotEmpty) {
    return (
      id: id,
      creation: remoteId == null
          ? null
          : _savedMultipart(value, source.sizeBytes),
    );
  }
  throw const TaskStoreFailure();
}

MultipartCreation _savedMultipart(Object? row, Object? sizeBytes) {
  if ((sizeBytes, row) case (
    final int size,
    {
      'remoteId': final String taskId,
      'partSizeBytes': final int partSizeBytes,
      'partCount': final int partCount,
      'expiresAt': final String expiresAt,
    },
  )) {
    final creation = MultipartCreation.checked(
      taskId: taskId,
      sizeBytes: size,
      partSizeBytes: partSizeBytes,
      partCount: partCount,
      expiresAt: expiresAt,
    );
    if (creation != null) return creation;
  }
  throw const TaskStoreFailure();
}

String _multipartStatus(Map<Object?, Object?> row) {
  final plan = _savedMultipart(row, row['sizeBytes']);
  final confirmed = row['confirmedParts'];
  if (confirmed is! int || confirmed < 0 || confirmed > plan.partCount) {
    throw const TaskStoreFailure();
  }
  final progress = confirmed == plan.partCount
      ? '已确认 $confirmed/${plan.partCount} 片，待确认发布'
      : '已确认 $confirmed/${plan.partCount} 片';
  return '$progress · 任务 ${plan.taskId}';
}

Future<Object?> _reserveUpload(
  AccountOwner owner,
  String parentId,
  PreparedUploadFile source,
) => _taskCall<Object?>('reserveUpload', owner, {
  'parentId': parentId,
  'name': source.name,
  'sizeBytes': source.sizeBytes,
  'sha256': source.sha256,
});

Future<void> saveMultipartCheckpoint(
  AccountOwner owner,
  String id,
  MultipartCreation creation,
) => _taskCall<void>('checkpointMultipart', owner, {
  'id': id,
  'remoteId': creation.taskId,
  'sizeBytes': creation.sizeBytes,
  'partSizeBytes': creation.partSizeBytes,
  'partCount': creation.partCount,
  'expiresAt': creation.expiresAt,
});

Future<Map<int, MultipartReceipt>> readMultipartReceipts(
  AccountOwner owner,
  String id,
  MultipartCreation plan,
) async {
  final rows = await _taskCall<List<Object?>>('readMultipartParts', owner, {
    'id': id,
    'remoteId': plan.taskId,
  });
  if (rows == null || rows.length > plan.partCount) {
    throw const TaskStoreFailure();
  }
  final receipts = <int, MultipartReceipt>{};
  for (final row in rows) {
    if (row
        case {
          'number': final int number,
          'sizeBytes': final int size,
          'sha256': final String sha256,
          'etag': final String etag,
        }
        when number >= 1 &&
            number <= plan.partCount &&
            size == plan.sizeOf(number) &&
            isMultipartSha256(sha256) &&
            isMultipartETag(etag) &&
            !receipts.containsKey(number)) {
      receipts[number] = (
        number: number,
        sizeBytes: size,
        sha256: sha256,
        etag: etag,
      );
    } else {
      throw const TaskStoreFailure();
    }
  }
  return receipts;
}

Future<void> saveMultipartPart(
  AccountOwner owner,
  String id,
  String taskId,
  MultipartReceipt receipt,
) => _taskCall<void>('recordMultipartPart', owner, {
  'id': id,
  'remoteId': taskId,
  ..._receiptArguments(receipt),
});

Future<void> saveReconciledMultipartParts(
  AccountOwner owner,
  String id,
  String taskId,
  Iterable<MultipartReceipt> receipts,
) => _taskCall<void>('reconcileMultipartParts', owner, {
  'id': id,
  'remoteId': taskId,
  'receipts': receipts.map(_receiptArguments).toList(),
});

Map<String, Object> _receiptArguments(MultipartReceipt receipt) => {
  'number': receipt.number,
  'sizeBytes': receipt.sizeBytes,
  'sha256': receipt.sha256,
  'etag': receipt.etag,
};

Future<void> saveMultipartOutcome(
  AccountOwner owner,
  String id,
  String taskId,
  MultipartTask task,
) => _taskCall<void>('saveMultipartOutcome', owner, {
  'id': id,
  'remoteId': taskId,
  'state': task.state,
  'entryId': ?task.entryId,
  if (task.state != 'COMPLETED') 'errorCode': ?task.errorCode,
});

Future<void> saveSingleCheckpoint(
  AccountOwner owner,
  String id,
  String remoteId,
  String state, {
  String? entryId,
}) => _taskCall<void>('checkpointSingle', owner, {
  'id': id,
  'remoteId': remoteId,
  'state': state,
  'entryId': ?entryId,
});

Future<void> recordSavedDownload(
  AccountOwner owner,
  String actualName,
  DownloadGrant grant,
) => _taskCall<void>('recordDownload', owner, {
  'name': actualName,
  'sizeBytes': grant.sizeBytes,
  'sha256': grant.sha256,
});

Future<T?> _taskCall<T>(
  String operation,
  AccountOwner owner,
  Map<String, Object> arguments,
) async {
  try {
    return await _tasks.invokeMethod<T>(operation, {
      'server': owner.server,
      'userId': owner.userId,
      ...arguments,
    });
  } on MissingPluginException {
    throw const TaskStoreFailure();
  } on PlatformException {
    throw const TaskStoreFailure();
  }
}

Future<List<MobileTask>> readMobileTasks(AccountOwner owner) =>
    _loadTasks('list', owner);

Future<List<MobileTask>> createMobileTestTask(AccountOwner owner) =>
    _loadTasks('createTest', owner);

Future<List<MobileTask>> _loadTasks(
  String operation,
  AccountOwner owner,
) async {
  // 1. 服务和用户来自当前会话。Android 按这两个字段读写 SQLite。
  final List<Object?>? rows;
  try {
    rows = await _tasks.invokeListMethod<Object?>(operation, {
      'server': owner.server,
      'userId': owner.userId,
    });
  } on MissingPluginException {
    throw const TaskStoreFailure();
  } on PlatformException {
    throw const TaskStoreFailure();
  }

  // 2. 空数组是有效结果。完整解析后再交付界面。
  if (rows == null) throw const TaskStoreFailure();
  return rows.map(MobileTask.fromAndroid).toList();
}
