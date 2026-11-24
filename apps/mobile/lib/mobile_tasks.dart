import 'package:flutter/services.dart';

import 'account_owner.dart';
import 'native_login.dart';
import 'native_download.dart';
import 'native_single_upload.dart';

const _tasks = MethodChannel('cloudnest/android_tasks');

class TaskStoreFailure implements Exception {
  const TaskStoreFailure();
}

class MobileTask {
  const MobileTask(this.id, this.name, this.createdAt, this.statusLabel);

  factory MobileTask.fromAndroid(Object? row) {
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
        ('UPLOAD', 'PENDING') => '待确认',
        ('UPLOAD', 'COMPLETED') => '已上传',
        ('UPLOAD', 'FAILED') => '失败',
        ('DOWNLOAD', 'COMPLETED') => '已下载',
        _ => throw const TaskStoreFailure(),
      };
      return MobileTask(
        id,
        name,
        DateTime.fromMillisecondsSinceEpoch(createdAtMs),
        label,
      );
    }
    throw const TaskStoreFailure();
  }

  final String id;
  final String name;
  final DateTime createdAt;
  final String statusLabel;
}

typedef LocalSingleIntent = ({String id, String? remoteId});

Future<LocalSingleIntent> reserveSingleIntent(
  AccountOwner owner,
  String parentId,
  PreparedUploadFile source,
) async {
  final value = await _taskCall<Object?>('reserveSingle', owner, {
    'parentId': parentId,
    'name': source.name,
    'sizeBytes': source.sizeBytes,
    'sha256': source.sha256,
  });
  if (value case {'id': final String id, 'remoteId': final String? remoteId}
      when id.isNotEmpty && (remoteId == null || isCloudNestId(remoteId))) {
    return (id: id, remoteId: remoteId);
  }
  throw const TaskStoreFailure();
}

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
