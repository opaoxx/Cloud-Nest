import 'dart:convert';
import 'dart:io';

import 'package:cloudnest/native_login.dart';
import 'package:cloudnest/native_upload_reuse.dart';
import 'package:flutter_test/flutter_test.dart';

const _sha = 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855';

void main() {
  test('only the explicit content miss allows normal upload', () async {
    expect(
      await _reuse(404, {
        'error': {'code': 'REUSABLE_CONTENT_NOT_FOUND'},
      }),
      isNull,
    );
    for (final response in [
      (404, 'ENTRY_NOT_FOUND'),
      (507, 'QUOTA_EXCEEDED'),
      (409, 'IDEMPOTENCY_CONFLICT'),
      (500, 'INTERNAL_ERROR'),
    ]) {
      await expectLater(
        _reuse(response.$1, {
          'error': {'code': response.$2},
        }),
        throwsA(isA<AccountFailure>()),
      );
    }
  });

  test('requires the original directory and retains the actual name', () async {
    final entry = {
      'id': '8',
      'parent_id': '2',
      'name': '样本 (1).bin',
      'entry_type': 'FILE',
      'size_bytes': 0,
      'revision': '1',
      'status': 'ACTIVE',
    };
    expect(
      await _reuse(201, {
        'data': {'entry': entry},
      }),
      (entryId: '8', name: '样本 (1).bin'),
    );
    entry['parent_id'] = '9';
    await expectLater(
      _reuse(201, {
        'data': {'entry': entry},
      }),
      throwsA(isA<AccountFailure>()),
    );
  });
}

Future<UploadReuseReceipt?> _reuse(int status, Map<String, Object> body) async {
  final server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
  server.listen((request) async {
    expect(request.uri.path, '/api/v1/files/reuse');
    expect(request.headers.value('Idempotency-Key'), 'original-key');
    expect(jsonDecode(await utf8.decoder.bind(request).join()), {
      'parent_id': '2',
      'name': '样本.bin',
      'size_bytes': 0,
      'sha256': _sha,
    });
    request.response.statusCode = status;
    request.response.headers.contentType = ContentType.json;
    request.response.write(jsonEncode(body));
    await request.response.close();
  });
  try {
    return await reuseUploadContent(
      NativeSession(
        address: Uri(scheme: 'http', host: '127.0.0.1', port: server.port),
        username: 'root',
        userId: '1',
        accessToken: 'fixture-access',
        refreshToken: 'fixture-refresh',
      ),
      'original-key',
      '2',
      (name: '样本.bin', sizeBytes: 0, sha256: _sha),
    );
  } finally {
    await server.close(force: true);
  }
}
