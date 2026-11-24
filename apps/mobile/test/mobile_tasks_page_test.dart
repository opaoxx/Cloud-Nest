import 'package:cloudnest/mobile_account.dart';
import 'package:cloudnest/mobile_tasks_page.dart';
import 'package:cloudnest/native_multipart_upload.dart';
import 'package:cloudnest/transfer_pause.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:material_ui/material_ui.dart';

const _channel = MethodChannel('cloudnest/android_tasks');
const _owner = (
  server: 'https://cloud.example.test',
  userId: '18446744073709551615',
);
const _taskId = '6dd564db-8d95-4e29-b011-0dc8cff7b2de';

void main() {
  testWidgets('creates the scoped record on retry and reads it on reopening', (
    tester,
  ) async {
    final messenger =
        TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger;
    addTearDown(() => messenger.setMockMethodCallHandler(_channel, null));
    final operations = <String>[];
    var saved = false;
    var failCreate = true;
    messenger.setMockMethodCallHandler(_channel, (call) async {
      expect(call.arguments, {
        'server': _owner.server,
        'userId': _owner.userId,
      });
      operations.add(call.method);
      if (call.method == 'createTest') {
        if (failCreate) {
          failCreate = false;
          throw PlatformException(code: 'task_store_unavailable');
        }
        saved = true;
      } else {
        expect(call.method, 'list');
      }
      return [
        if (saved)
          {
            'id': _taskId,
            'name': '本地测试任务',
            'kind': 'TEST',
            'state': 'TEST_RECORD',
            'createdAtMs': 1795478400000,
          },
      ];
    });

    await tester.pumpWidget(
      MaterialApp(
        home: MobileTasksPage(owner: _owner, account: MobileAccount()),
      ),
    );
    await tester.pumpAndSettle();
    expect(find.text('暂无任务记录'), findsOneWidget);
    expect(operations, ['list']);

    await tester.tap(find.text('创建测试任务'));
    await tester.pumpAndSettle();
    expect(find.text('无法创建测试任务。'), findsOneWidget);
    await tester.tap(find.text('重试'));
    await tester.pumpAndSettle();
    expect(find.byKey(const ValueKey(_taskId)), findsOneWidget);
    expect(find.textContaining('未执行'), findsOneWidget);
    expect(find.text('创建测试任务'), findsNothing);

    await tester.pumpWidget(const SizedBox.shrink());
    await tester.pumpWidget(
      MaterialApp(
        home: MobileTasksPage(owner: _owner, account: MobileAccount()),
      ),
    );
    await tester.pumpAndSettle();
    expect(find.byKey(const ValueKey(_taskId)), findsOneWidget);
    expect(operations, ['list', 'createTest', 'createTest', 'list']);
  });

  testWidgets('reselects the source for the same paused task', (tester) async {
    final account = _ResumeAccount();
    final messenger =
        TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger;
    addTearDown(() => messenger.setMockMethodCallHandler(_channel, null));
    messenger.setMockMethodCallHandler(
      _channel,
      (call) async => [
        {
          'id': _taskId,
          'name': '大文件.bin',
          'kind': 'UPLOAD',
          'state': 'PENDING',
          'createdAtMs': 1795478400000,
          'sizeBytes': 16777217,
        },
      ],
    );
    await tester.pumpWidget(
      MaterialApp(
        home: MobileTasksPage(owner: _owner, account: account),
      ),
    );
    await tester.pumpAndSettle();
    await tester.tap(find.text('继续上传'));
    await tester.pumpAndSettle();
    expect(find.textContaining('授权已失效'), findsOneWidget);
    expect(find.text('重新选择文件'), findsOneWidget);
    await tester.tap(find.text('重新选择文件'));
    await tester.pumpAndSettle();
    expect(find.text('已取消重新选择。任务保持暂停。'), findsOneWidget);
    expect(account.attempts, [(_taskId, false), (_taskId, true)]);
  });
}

class _ResumeAccount extends MobileAccount {
  final attempts = <(String, bool)>[];

  @override
  Future<String?> resumeMultipart(
    AccountOwner taskOwner,
    String localId, {
    required bool reselect,
    required void Function(MultipartCreation plan, int confirmed) onProgress,
    TransferPause? pause,
  }) async {
    expect(taskOwner, _owner);
    attempts.add((localId, reselect));
    if (!reselect) throw const MultipartSourceUnavailable('授权已失效。');
    return null;
  }
}
