import 'dart:convert';
import 'dart:io';

import 'package:cloudnest/native_directory.dart';
import 'package:cloudnest/native_login.dart';
import 'package:flutter_test/flutter_test.dart';

void main() {
  test(
    'validates rename, destination of move, and the separate trash snapshot',
    () async {
      final server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
      addTearDown(() => server.close(force: true));
      final requests = <(String, String, Object?)>[];
      server.listen((request) async {
        final payload = jsonDecode(await utf8.decoder.bind(request).join());
        requests.add((request.method, request.uri.path, payload));
        final trash = request.uri.path.endsWith('/trash');
        final move = request.uri.path.endsWith('/move');
        request.response.headers.contentType = ContentType.json;
        request.response.write(
          jsonEncode({
            'data': {
              'entry': {
                'id': '2',
                'parent_id': move || trash ? '10' : '1',
                'name': '改名.txt',
                'revision': trash ? '4' : (move ? '3' : '2'),
                'status': trash ? 'TRASHED' : 'ACTIVE',
                if (trash) ...{
                  'entry_type': 'FILE',
                  'deleted_at': '2026-11-24T00:00:00Z',
                } else ...{
                  'kind': 'document',
                  'size_bytes': 17,
                },
              },
            },
          }),
        );
        await request.response.close();
      });
      final session = NativeSession(
        address: Uri.parse('http://127.0.0.1:${server.port}'),
        username: 'root',
        userId: '1',
        accessToken: 'fixture',
        refreshToken: 'fixture-refresh',
      );
      final renamed = await changeDirectoryFile(
        session,
        (
          id: '2',
          name: '原文件.txt',
          isFolder: false,
          sizeBytes: 17,
          revision: '1',
        ),
        '1',
        'rename-fixture',
        name: '改名.txt',
      );
      expect(renamed.name, '改名.txt');
      expect(renamed.revision, '2');
      await expectLater(
        changeDirectoryFile(
          session,
          renamed,
          '1',
          'wrong-parent',
          targetParentId: '9',
        ),
        throwsA(isA<AccountFailure>()),
      );
      final moved = await changeDirectoryFile(
        session,
        renamed,
        '1',
        'move-fixture',
        targetParentId: '10',
      );
      expect(moved.revision, '3');
      final trashed = await changeDirectoryFile(
        session,
        moved,
        '10',
        'trash-fixture',
      );
      expect(trashed.revision, '4');
      expect(requests, hasLength(4));
      final (renameMethod, renamePath, renameBody) = requests[0];
      final (moveMethod, movePath, moveBody) = requests[2];
      final (trashMethod, trashPath, trashBody) = requests[3];
      expect((renameMethod, renamePath), ('PATCH', '/api/v1/entries/2'));
      expect(renameBody, {'expected_revision': '1', 'name': '改名.txt'});
      expect((moveMethod, movePath), ('POST', '/api/v1/entries/2/move'));
      expect(moveBody, {'expected_revision': '2', 'target_parent_id': '10'});
      expect((trashMethod, trashPath), ('POST', '/api/v1/entries/2/trash'));
      expect(trashBody, {'expected_revision': '3'});
    },
  );
}
