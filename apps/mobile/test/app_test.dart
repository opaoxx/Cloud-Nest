import 'package:cloudnest/main.dart';
import 'package:flutter/services.dart';
import 'package:material_ui/material_ui.dart';
import 'package:flutter_test/flutter_test.dart';

void main() {
  testWidgets('CloudNest renders its launch screen', (tester) async {
    await tester.pumpWidget(const CloudNestApp());

    expect(find.text('CloudNest'), findsOneWidget);
  });

  testWidgets('applies and restores the selected app appearance', (
    tester,
  ) async {
    const appearance = MethodChannel('cloudnest/android_appearance');
    const credentials = MethodChannel('cloudnest/android_session');
    final messenger =
        TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger;
    messenger.setMockMethodCallHandler(credentials, (_) async => null);
    addTearDown(() => messenger.setMockMethodCallHandler(credentials, null));
    addTearDown(() => messenger.setMockMethodCallHandler(appearance, null));
    String? stored;
    messenger.setMockMethodCallHandler(appearance, (call) async {
      if (call.method == 'readTheme') return stored;
      expect(call.method, 'writeTheme');
      stored = call.arguments as String;
      return null;
    });
    ThemeMode mode() =>
        tester.widget<MaterialApp>(find.byType(MaterialApp)).themeMode!;
    Future<void> choose(String name) async {
      await tester.tap(find.byTooltip('界面主题'));
      await tester.pumpAndSettle();
      await tester.tap(find.text(name));
      await tester.pumpAndSettle();
    }

    await tester.pumpWidget(const CloudNestApp());
    await tester.pumpAndSettle();
    expect(mode(), ThemeMode.system);
    await choose('浅色');
    expect(mode(), ThemeMode.light);
    await choose('深色');
    expect(mode(), ThemeMode.dark);
    expect(
      Theme.of(tester.element(find.byType(Scaffold))).brightness,
      Brightness.dark,
    );
    expect(stored, 'dark');
    await tester.pumpWidget(const SizedBox.shrink());
    await tester.pumpWidget(const CloudNestApp());
    await tester.pumpAndSettle();
    expect(mode(), ThemeMode.dark);
    await choose('跟随系统');
    expect(mode(), ThemeMode.system);
    expect(stored, 'system');
  });

  testWidgets('login advances IPv4 segments and rejects invalid addresses', (
    tester,
  ) async {
    const credentials = MethodChannel('cloudnest/android_session');
    final messenger =
        TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger;
    messenger.setMockMethodCallHandler(credentials, (call) async {
      expect(call.method, 'read');
      return null;
    });
    addTearDown(() => messenger.setMockMethodCallHandler(credentials, null));

    await tester.pumpWidget(const CloudNestApp());
    await tester.pumpAndSettle();
    expect(
      tester
          .widget<TextButton>(find.widgetWithText(TextButton, '登录'))
          .onPressed,
      isNotNull,
    );
    await tester.tap(find.text('登录'));
    await tester.pumpAndSettle();

    final first = find.byKey(const ValueKey('server-ip-0'));
    final second = find.byKey(const ValueKey('server-ip-1'));
    expect(find.text('http://'), findsOneWidget);
    expect(find.text('.'), findsNWidgets(3));
    expect(find.text('18081'), findsOneWidget);
    await tester.enterText(first, '192.');
    await tester.pump();
    expect(tester.widget<TextField>(first).controller!.text, '192');
    expect(tester.widget<TextField>(second).focusNode!.hasFocus, isTrue);
    await tester.enterText(second, '256');
    await tester.tap(find.widgetWithText(FilledButton, '登录'));
    await tester.pumpAndSettle();

    expect(find.textContaining('请输入有效的四段 IPv4 地址'), findsOneWidget);
    final login = tester.widget<FilledButton>(
      find.widgetWithText(FilledButton, '登录'),
    );
    expect(login.onPressed, isNotNull);
    expect(find.text('选择文件'), findsNothing);
  });
}
