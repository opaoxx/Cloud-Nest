import 'package:cloudnest/main.dart';
import 'package:material_ui/material_ui.dart';
import 'package:flutter_test/flutter_test.dart';

void main() {
  testWidgets('CloudNest renders its launch screen', (tester) async {
    await tester.pumpWidget(const CloudNestApp());

    expect(find.text('CloudNest'), findsOneWidget);
  });

  testWidgets('login rejects a non-root address and allows another attempt', (
    tester,
  ) async {
    await tester.pumpWidget(const CloudNestApp());
    await tester.tap(find.text('登录'));
    await tester.pumpAndSettle();

    await tester.enterText(
      find.byType(TextField).first,
      'https://cloud.example.com/unexpected',
    );
    await tester.tap(find.widgetWithText(FilledButton, '登录'));
    await tester.pumpAndSettle();

    expect(find.textContaining('请输入服务根地址'), findsOneWidget);
    final login = tester.widget<FilledButton>(
      find.widgetWithText(FilledButton, '登录'),
    );
    expect(login.onPressed, isNotNull);
    expect(find.text('选择文件'), findsNothing);
  });
}
