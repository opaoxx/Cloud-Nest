import 'package:cloudnest/cloud_drive_page.dart';
import 'package:cloudnest/mobile_account.dart';
import 'package:cloudnest/native_directory.dart';
import 'package:cloudnest/native_login.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:material_ui/material_ui.dart';

void main() {
  testWidgets('retries a page with the same cursor and merges by entry ID', (
    tester,
  ) async {
    final account = _DirectoryAccount();
    await tester.pumpWidget(
      MaterialApp(home: CloudDrivePage(account: account)),
    );
    await tester.pumpAndSettle();
    expect(find.text('根目录'), findsOneWidget);
    expect(tester.getTopLeft(find.text('根目录')).dx, lessThan(40));
    expect(find.text('notes.txt'), findsOneWidget);

    await tester.tap(find.text('加载更多'));
    await tester.pumpAndSettle();
    expect(find.text('测试网络错误'), findsOneWidget);
    expect(find.text('notes.txt'), findsOneWidget);

    await tester.tap(find.text('重试'));
    await tester.pumpAndSettle();
    expect(find.text('notes.txt'), findsOneWidget);
    expect(find.text('report.txt'), findsOneWidget);
    expect(find.text('加载更多'), findsNothing);
    expect(account.requests, [
      (_rootId, null),
      (_rootId, _cursor),
      (_rootId, _cursor),
    ]);
  });
}

const _rootId = '18446744073709551614';
const _cursor = 'cursor+/%=';

class _DirectoryAccount extends MobileAccount {
  final requests = <(String, String?)>[];

  @override
  Future<CurrentProfile> currentProfile() async =>
      (userId: '17', username: 'test', rootEntryId: _rootId);

  @override
  Future<DirectoryListing> listDirectory({
    required String rootId,
    required String parentId,
    String? cursor,
  }) async {
    expect(rootId, _rootId);
    requests.add((parentId, cursor));
    if (requests.length == 2) throw const AccountFailure('测试网络错误');
    return (
      breadcrumbs: [(id: _rootId, name: '')],
      entries: [
        (id: '81', name: 'notes.txt', isFolder: false, sizeBytes: 14),
        if (cursor != null)
          (id: '82', name: 'report.txt', isFolder: false, sizeBytes: 3),
      ],
      nextCursor: cursor == null ? _cursor : null,
    );
  }

  @override
  AccountOwner? get owner => (server: 'https://example.test', userId: '17');
}
