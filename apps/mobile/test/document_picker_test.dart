import 'dart:async';

import 'package:cloudnest/main.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:material_ui/material_ui.dart';

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();
  const channel = MethodChannel('cloudnest/android_document');
  final messenger =
      TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger;
  final pickButton = find.widgetWithText(FilledButton, '选择文件');

  tearDown(() => messenger.setMockMethodCallHandler(channel, null));

  testWidgets('shows the document summary and returns to idle on cancel', (
    tester,
  ) async {
    const digest =
        'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855';
    var calls = 0;
    final cancelled = Completer<Object?>();
    messenger.setMockMethodCallHandler(channel, (call) async {
      expect(call.method, 'pickAndSummarize');
      calls++;
      if (calls == 1) {
        return {'name': '空文档.txt', 'sizeBytes': 0, 'sha256': digest};
      }
      return cancelled.future;
    });

    await tester.pumpWidget(const CloudNestApp());
    await tester.tap(pickButton);
    await tester.pumpAndSettle();
    expect(find.text('空文档.txt'), findsOneWidget);
    expect(find.text('大小 0 B'), findsOneWidget);
    expect(find.text('SHA-256 $digest'), findsOneWidget);

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
    messenger.setMockMethodCallHandler(channel, (_) async {
      throw PlatformException(code: 'document_read_failed');
    });

    await tester.pumpWidget(const CloudNestApp());
    await tester.tap(pickButton);
    await tester.pumpAndSettle();
    expect(find.text('无法读取所选文件，请重新选择。'), findsOneWidget);
    expect(tester.widget<FilledButton>(pickButton).onPressed, isNotNull);
  });
}
