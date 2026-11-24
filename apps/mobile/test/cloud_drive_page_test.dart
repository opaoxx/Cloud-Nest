import 'dart:async';

import 'package:cloudnest/cloud_drive_page.dart';
import 'package:cloudnest/mobile_account.dart';
import 'package:cloudnest/native_directory.dart';
import 'package:cloudnest/native_login.dart';
import 'package:cloudnest/native_upload_creation.dart';
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

  testWidgets('uploads to the confirmed folder and reloads after publication', (
    tester,
  ) async {
    final account = _UploadAccount();
    await tester.pumpWidget(
      MaterialApp(
        home: CloudDrivePage(account: account, source: _source),
      ),
    );
    await tester.pumpAndSettle();
    await tester.tap(find.text('目标目录'));
    await tester.pumpAndSettle();
    await tester.tap(find.text('上传到此目录'));
    await tester.pumpAndSettle();
    expect(find.text('根目录 / 目标目录'), findsOneWidget);
    expect(account.target, isNull);

    await tester.tap(find.text('上传'));
    await tester.pump();
    await tester.pump(const Duration(milliseconds: 230));
    expect(account.target, '81');
    expect(find.text('上传成功。'), findsNothing);
    expect(
      tester
          .widget<FilledButton>(find.widgetWithText(FilledButton, '上传到此目录'))
          .onPressed,
      isNull,
    );

    account.published.complete('82');
    await tester.pumpAndSettle();
    expect(find.text('上传成功。'), findsOneWidget);
    expect(find.text('样本.txt'), findsOneWidget);
  });

  testWidgets('downloads the selected revision and shows the provider name', (
    tester,
  ) async {
    final account = _DownloadAccount();
    await tester.pumpWidget(
      MaterialApp(home: CloudDrivePage(account: account)),
    );
    await tester.pumpAndSettle();
    final action = find.byWidgetPredicate(
      (widget) => widget is IconButton && widget.tooltip == '下载 notes.txt',
    );
    await tester.tap(action);
    await tester.pump();
    expect(account.selected?.revision, '1');
    expect(tester.widget<IconButton>(action).onPressed, isNull);
    account.saved.complete('notes (1).txt');
    await tester.pumpAndSettle();
    expect(find.text('已保存「notes (1).txt」。'), findsOneWidget);
    expect(tester.widget<IconButton>(action).onPressed, isNotNull);
  });
}

class _DownloadAccount extends _DirectoryAccount {
  final saved = Completer<String?>();
  DirectoryEntry? selected;

  @override
  Future<String?> downloadFile(DirectoryEntry entry) {
    selected = entry;
    return saved.future;
  }
}

const _source = (
  name: '样本.txt',
  sizeBytes: 0,
  sha256: 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855',
);

class _UploadAccount extends _DirectoryAccount {
  final published = Completer<String>();
  String? target;

  @override
  Future<String> uploadSingle(PreparedUploadFile source, String parentId) {
    expect(source, _source);
    target = parentId;
    return published.future;
  }

  @override
  Future<DirectoryListing> listDirectory({
    required String rootId,
    required String parentId,
    String? cursor,
  }) async => (
    breadcrumbs: [
      (id: _rootId, name: ''),
      if (parentId == '81') (id: '81', name: '目标目录'),
    ],
    entries: [
      if (parentId == _rootId)
        (id: '81', name: '目标目录', isFolder: true, sizeBytes: 0, revision: '1'),
      if (parentId == '81' && published.isCompleted)
        (
          id: '82',
          name: _source.name,
          isFolder: false,
          sizeBytes: 0,
          revision: '1',
        ),
    ],
    nextCursor: null,
  );
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
        (
          id: '81',
          name: 'notes.txt',
          isFolder: false,
          sizeBytes: 14,
          revision: '1',
        ),
        if (cursor != null)
          (
            id: '82',
            name: 'report.txt',
            isFolder: false,
            sizeBytes: 3,
            revision: '1',
          ),
      ],
      nextCursor: cursor == null ? _cursor : null,
    );
  }

  @override
  AccountOwner? get owner => (server: 'https://example.test', userId: '17');
}
