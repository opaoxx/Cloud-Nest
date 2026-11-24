import 'dart:convert';
import 'dart:io';

import 'package:cloudnest/native_login.dart';
import 'package:cloudnest/native_multipart_reconcile.dart';
import 'package:cloudnest/native_multipart_upload.dart';
import 'package:cloudnest/native_upload_creation.dart';
import 'package:flutter_test/flutter_test.dart';

void main() {
  test(
    'keeps only parts with the same length and content as the source',
    () async {
      final receipts = await _read([
        _row(1, maxSingleUploadBytes, _checksum),
        _row(2, maxSingleUploadBytes, base64Encode(List.filled(32, 0xcd))),
        _row(3, 2, _checksum),
      ]);
      expect(receipts.keys, [1]);
      expect(receipts[1]?.sha256, _digest);
      expect(receipts[1]?.etag, 'etag-1');
    },
  );

  test(
    'a malformed checksum after a valid receipt invalidates the whole list',
    () async {
      await expectLater(
        _read([
          _row(1, maxSingleUploadBytes, _checksum),
          _row(2, maxSingleUploadBytes, 'invalid'),
        ]),
        throwsA(isA<AccountFailure>()),
      );
    },
  );
}

final _checksum = base64Encode(List.filled(32, 0xab));
final _digest = List.filled(32, 'ab').join();

Map<String, Object> _row(int number, int size, String checksum) => {
  'part_number': number,
  'size_bytes': size,
  'checksum_sha256': checksum,
  'etag': 'etag-$number',
};

Future<Map<int, MultipartReceipt>> _read(
  List<Map<String, Object>> parts,
) async {
  final server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
  server.listen((request) async {
    request.response.headers.contentType = ContentType.json;
    request.response.write(
      jsonEncode({
        'data': {
          'part_size_bytes': maxSingleUploadBytes,
          'part_count': 3,
          'parts': parts,
        },
      }),
    );
    await request.response.close();
  });
  final plan = MultipartCreation.checked(
    taskId: '8',
    sizeBytes: 2 * maxSingleUploadBytes + 1,
    partSizeBytes: maxSingleUploadBytes,
    partCount: 3,
    expiresAt: '2030-01-01T00:00:00Z',
  )!;
  try {
    return await reconcileMultipartReceipts(
      NativeSession(
        address: Uri(scheme: 'http', host: '127.0.0.1', port: server.port),
        username: 'root',
        userId: '1',
        accessToken: 'test-access-token',
        refreshToken: 'test-refresh-token',
      ),
      plan,
      List.filled(3, _digest),
    );
  } finally {
    await server.close(force: true);
  }
}
