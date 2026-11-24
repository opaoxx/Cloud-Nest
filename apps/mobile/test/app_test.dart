import 'package:cloudnest/main.dart';
import 'package:flutter_test/flutter_test.dart';

void main() {
  testWidgets('CloudNest renders its launch screen', (tester) async {
    await tester.pumpWidget(const CloudNestApp());

    expect(find.text('CloudNest'), findsOneWidget);
  });
}
