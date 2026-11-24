import 'package:flutter/services.dart';

import 'mobile_account.dart';

const _tasks = MethodChannel('cloudnest/android_tasks');

class TaskStoreFailure implements Exception {
  const TaskStoreFailure();
}

class MobileTask {
  const MobileTask(this.id, this.name, this.createdAt);

  factory MobileTask.fromAndroid(Object? row) {
    if (row
        case {
          'id': final String id,
          'name': final String name,
          'kind': 'TEST',
          'state': 'TEST_RECORD',
          'createdAtMs': final int createdAtMs,
        }
        when id.isNotEmpty &&
            name.isNotEmpty &&
            createdAtMs >= 0 &&
            createdAtMs <= 8640000000000000) {
      return MobileTask(
        id,
        name,
        DateTime.fromMillisecondsSinceEpoch(createdAtMs),
      );
    }
    throw const TaskStoreFailure();
  }

  final String id;
  final String name;
  final DateTime createdAt;
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
