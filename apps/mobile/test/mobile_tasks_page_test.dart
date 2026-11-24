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
  testWidgets(
    'cancels only after explicit confirmation and shows the actual result',
    (tester) async {
      final account = _CancelAccount();
      final messenger =
          TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger;
      addTearDown(() => messenger.setMockMethodCallHandler(_channel, null));
      messenger.setMockMethodCallHandler(
        _channel,
        (call) async => [
          {
            'id': _taskId,
            'name': '原文件.bin',
            'kind': 'UPLOAD',
            'state': account.submits == 0 ? 'PENDING' : 'COMPLETED',
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
      await tester.tap(find.byTooltip('取消上传 原文件.bin'));
      await tester.pumpAndSettle();
      await tester.tap(find.text('保留任务'));
      await tester.pumpAndSettle();
      expect(account.submits, 0);
      await tester.tap(find.byTooltip('取消上传 原文件.bin'));
      await tester.pumpAndSettle();
      await tester.tap(find.text('确认取消'));
      await tester.pumpAndSettle();
      expect(account.submits, 1);
      expect(find.text('上传已发布，可在原目录查看。'), findsOneWidget);
      expect(find.textContaining('已上传'), findsOneWidget);
      expect(find.byTooltip('取消上传 原文件.bin'), findsNothing);
    },
  );

  testWidgets('reads only transfer records and retries a local read failure', (
    tester,
  ) async {
    final messenger =
        TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger;
    addTearDown(() => messenger.setMockMethodCallHandler(_channel, null));
    var reads = 0;
    messenger.setMockMethodCallHandler(_channel, (call) async {
      expect(call.method, 'list');
      expect(call.arguments, {
        'server': _owner.server,
        'userId': _owner.userId,
      });
      if (++reads == 1) throw PlatformException(code: 'task_store_unavailable');
      return [
        {
          'id': 'legacy',
          'name': '本地测试任务',
          'kind': 'TEST',
          'state': 'TEST_RECORD',
          'createdAtMs': 1795478400000,
        },
        {
          'id': _taskId,
          'name': '课程视频.mp4',
          'kind': 'UPLOAD',
          'state': 'PENDING',
          'createdAtMs': 1795478400000,
          'remoteId': '19',
          'sizeBytes': 16777217,
          'partSizeBytes': 16777216,
          'partCount': 2,
          'confirmedParts': 1,
          'expiresAt': '2030-01-01T00:00:00Z',
        },
      ];
    });
    await tester.pumpWidget(
      MaterialApp(
        home: MobileTasksPage(owner: _owner, account: MobileAccount()),
      ),
    );
    await tester.pumpAndSettle();
    expect(find.text('无法读取任务记录。'), findsOneWidget);
    await tester.tap(find.text('重试'));
    await tester.pumpAndSettle();
    expect(find.text('课程视频.mp4'), findsOneWidget);
    expect(find.textContaining('16 MB · 已上传 50%'), findsOneWidget);
    expect(find.textContaining('任务 19'), findsNothing);
    expect(find.text('本地测试任务'), findsNothing);
    expect(find.text('创建测试任务'), findsNothing);
    expect(reads, 2);
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

class _CancelAccount extends MobileAccount {
  int submits = 0;

  @override
  Future<String> cancelTransfer(
    AccountOwner taskOwner,
    String localId, {
    required bool submit,
  }) async {
    expect(taskOwner, _owner);
    expect(localId, _taskId);
    expect(submit, isTrue);
    submits++;
    return '上传已发布，可在原目录查看。';
  }
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
