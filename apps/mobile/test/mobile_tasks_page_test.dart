import 'package:cloudnest/mobile_tasks_page.dart';
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
      const MaterialApp(home: MobileTasksPage(owner: _owner)),
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
      const MaterialApp(home: MobileTasksPage(owner: _owner)),
    );
    await tester.pumpAndSettle();
    expect(find.byKey(const ValueKey(_taskId)), findsOneWidget);
    expect(operations, ['list', 'createTest', 'createTest', 'list']);
  });
}
