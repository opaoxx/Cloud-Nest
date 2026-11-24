import 'dart:async';

import 'package:cloudnest/main.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:material_ui/material_ui.dart';

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();
  const channel = MethodChannel('cloudnest/android_document');
  const credentials = MethodChannel('cloudnest/android_session');
  final messenger =
      TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger;
  final pickButton = find.widgetWithText(FilledButton, '选择文件');
  const emptyDigest =
      'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855';
  const emptyDocument = {
    'name': '空文档.txt',
    'sizeBytes': 0,
    'sha256': emptyDigest,
    'persisted': true,
  };

  setUp(() {
    messenger.setMockMethodCallHandler(credentials, (call) async {
      expect(call.method, 'read');
      return null;
    });
  });
  tearDown(() {
    messenger.setMockMethodCallHandler(channel, null);
    messenger.setMockMethodCallHandler(credentials, null);
  });

  testWidgets('shows the document summary and returns to idle on cancel', (
    tester,
  ) async {
    var calls = 0;
    final cancelled = Completer<Object?>();
    messenger.setMockMethodCallHandler(channel, (call) async {
      expect(call.arguments, 'guest');
      if (call.method == 'restoreDocument') return null;
      expect(call.method, 'pickAndSummarize');
      calls++;
      if (calls == 1) {
        return emptyDocument;
      }
      return cancelled.future;
    });

    await tester.pumpWidget(const CloudNestApp());
    await tester.pumpAndSettle();
    await tester.tap(pickButton);
    await tester.pumpAndSettle();
    expect(find.text('空文档.txt'), findsOneWidget);
    expect(find.text('大小 0 B'), findsOneWidget);
    expect(find.text('SHA-256 $emptyDigest'), findsOneWidget);

    await tester.tap(pickButton);
    await tester.pump();
    expect(tester.widget<FilledButton>(pickButton).onPressed, isNull);
    cancelled.complete(null);
    await tester.pumpAndSettle();
    expect(find.text('空文档.txt'), findsNothing);
    expect(find.text('正在选择或读取文件…'), findsNothing);
    expect(tester.widget<FilledButton>(pickButton).onPressed, isNotNull);
    expect(calls, 2);
  });

  testWidgets('provider read errors allow another selection', (tester) async {
    messenger.setMockMethodCallHandler(channel, (call) async {
      if (call.method == 'restoreDocument') return null;
      throw PlatformException(code: 'document_read_failed');
    });

    await tester.pumpWidget(const CloudNestApp());
    await tester.pumpAndSettle();
    await tester.tap(pickButton);
    await tester.pumpAndSettle();
    expect(find.text('无法读取所选文件，请重新选择。'), findsOneWidget);
    expect(tester.widget<FilledButton>(pickButton).onPressed, isNotNull);
  });

  testWidgets('rereads a remembered document at startup', (tester) async {
    messenger.setMockMethodCallHandler(channel, (call) async {
      expect(call.method, 'restoreDocument');
      return emptyDocument;
    });

    await tester.pumpWidget(const CloudNestApp());
    await tester.pumpAndSettle();
    expect(find.text('已重新读取上次授权的文件。'), findsOneWidget);
    expect(find.text('空文档.txt'), findsOneWidget);
    expect(tester.widget<FilledButton>(pickButton).onPressed, isNotNull);
  });

  testWidgets('allows reselection when the remembered read grant is gone', (
    tester,
  ) async {
    messenger.setMockMethodCallHandler(channel, (call) async {
      if (call.method == 'restoreDocument') {
        throw PlatformException(code: 'document_access_lost');
      }
      expect(call.method, 'pickAndSummarize');
      return {...emptyDocument, 'persisted': false};
    });

    await tester.pumpWidget(const CloudNestApp());
    await tester.pumpAndSettle();
    expect(find.text('上次文件的授权已失效，请重新选择。'), findsOneWidget);
    expect(find.text('空文档.txt'), findsNothing);
    await tester.tap(find.widgetWithText(FilledButton, '重新选择文件'));
    await tester.pumpAndSettle();
    expect(find.text('空文档.txt'), findsOneWidget);
    expect(find.text('已读取文件。本次授权无法在重启后恢复。'), findsOneWidget);
    expect(tester.widget<FilledButton>(pickButton).onPressed, isNotNull);
  });
}
