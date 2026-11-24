import 'dart:async';

import 'package:cloudnest/main.dart';
import 'package:cloudnest/mobile_account.dart';
import 'package:cloudnest/native_directory.dart';
import 'package:cloudnest/native_login.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:material_ui/material_ui.dart';

const _pickButton = ValueKey('choose-upload');
const _digest =
    'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855';
const _sample = {
  'name': '课程资料.txt',
  'sizeBytes': 1536,
  'sha256': _digest,
  'sourceMode': 'direct',
};
const AccountOwner _driveOwner = (server: 'https://example.test', userId: '17');

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();
  const channel = MethodChannel('cloudnest/android_document');
  const tasks = MethodChannel('cloudnest/android_tasks');
  final messenger =
      TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger;
  final pickButton = find.byKey(_pickButton);
  tearDown(() {
    messenger.setMockMethodCallHandler(channel, null);
    messenger.setMockMethodCallHandler(tasks, null);
  });

  testWidgets(
    'restored account opens the drive directly, with tasks in the app bar',
    (tester) async {
      final account = _RestoredAccount();
      messenger.setMockMethodCallHandler(tasks, (call) async {
        expect(call.method, 'list');
        return [];
      });
      await tester.pumpWidget(CloudNestApp(account: account));
      expect(find.text('正在恢复会话…'), findsOneWidget);
      expect(find.text('选择文件'), findsNothing);
      account.restored.complete();
      await tester.pumpAndSettle();
      expect(find.text('根目录'), findsOneWidget);
      expect(find.text('浏览云盘'), findsNothing);
      expect(pickButton, findsOneWidget);
      final history = find.byTooltip('任务记录');
      expect(
        tester.getCenter(history).dy,
        lessThan(tester.getCenter(pickButton).dy),
      );
      await tester.tap(history);
      await tester.pumpAndSettle();
      expect(find.text('暂无任务记录'), findsOneWidget);
    },
  );

  testWidgets(
    'opens a readable confirmation after picking and stays in place on cancel',
    (tester) async {
      final account = _RestoredAccount()..restored.complete();
      var picks = 0;
      messenger.setMockMethodCallHandler(channel, (call) async {
        expect(call.method, 'pickAndSummarize');
        expect(call.arguments, documentScopeFor(_driveOwner));
        return ++picks == 1 ? _sample : null;
      });
      await tester.pumpWidget(CloudNestApp(account: account));
      await tester.pumpAndSettle();
      await tester.tap(pickButton);
      await tester.pumpAndSettle();
      expect(find.text('课程资料.txt'), findsOneWidget);
      expect(find.text('1.5 KB'), findsOneWidget);
      expect(find.textContaining(_digest), findsNothing);
      await tester.tap(find.text('取消'));
      await tester.pumpAndSettle();

      await tester.tap(pickButton);
      await tester.pumpAndSettle();
      expect(picks, 2);
      expect(find.text('根目录'), findsOneWidget);
      expect(find.text('上传到当前目录'), findsNothing);
      expect(tester.widget<FilledButton>(pickButton).onPressed, isNotNull);
    },
  );

  testWidgets('space and read failures leave the fixed selector available', (
    tester,
  ) async {
    final account = _RestoredAccount()..restored.complete();
    var attempt = 0;
    messenger.setMockMethodCallHandler(channel, (call) async {
      throw PlatformException(
        code: ++attempt == 1
            ? 'document_staging_space'
            : 'document_read_failed',
      );
    });
    await tester.pumpWidget(CloudNestApp(account: account));
    await tester.pumpAndSettle();
    await tester.tap(pickButton);
    await tester.pumpAndSettle();
    expect(find.textContaining('手机可用空间不足'), findsOneWidget);
    await tester.tap(pickButton);
    await tester.pumpAndSettle();
    expect(find.text('无法读取所选文件，请重新选择。'), findsOneWidget);
    expect(tester.widget<FilledButton>(pickButton).onPressed, isNotNull);
  });
}

class _RestoredAccount extends MobileAccount {
  final restored = Completer<void>();
  AccountOwner? _owner;

  @override
  AccountOwner? get owner => _owner;

  @override
  Future<void> restore() async {
    await restored.future;
    _owner = _driveOwner;
  }

  @override
  Future<CurrentProfile> currentProfile() async =>
      (userId: '17', username: 'test', rootEntryId: '1');

  @override
  Future<DirectoryListing> listDirectory({
    required String rootId,
    required String parentId,
    String? cursor,
  }) async => (
    breadcrumbs: [(id: '1', name: '')],
    entries: <DirectoryEntry>[],
    nextCursor: null,
  );
}
