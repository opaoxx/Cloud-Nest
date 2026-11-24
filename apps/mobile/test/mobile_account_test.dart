import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:cloudnest/mobile_account.dart';
import 'package:cloudnest/native_login.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';

const _channel = MethodChannel('cloudnest/android_session');

class _LocalNetwork extends HttpOverrides {}

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();
  final messenger =
      TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger;
  String? saved;
  setUp(() {
    saved = null;
    messenger.setMockMethodCallHandler(_channel, (call) async {
      switch (call.method) {
        case 'read':
          return saved;
        case 'write':
          saved = call.arguments as String;
        case 'clear':
          saved = null;
        default:
          throw MissingPluginException();
      }
      return null;
    });
  });
  tearDown(() => messenger.setMockMethodCallHandler(_channel, null));

  test('concurrent and late expired reads share one saved rotation', () async {
    await HttpOverrides.runWithHttpOverrides(() async {
      final server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
      addTearDown(() => server.close(force: true));
      final refreshStarted = Completer<void>();
      final sendRefresh = Completer<void>();
      final sendLateExpiry = Completer<void>();
      final firstExpiries = Completer<void>();
      final firstRetries = Completer<void>();
      var oldRequests = 0;
      var expiredResponses = 0;
      var newRequests = 0;
      var refreshes = 0;

      server.listen((request) async {
        if (request.uri.path == '/api/v1/auth/refresh') {
          refreshes++;
          expect(request.method, 'POST');
          expect(request.headers.value('X-CloudNest-Client'), 'native');
          expect(request.headers.value('Origin'), isNull);
          expect(jsonDecode(await utf8.decoder.bind(request).join()), {
            'client_type': 'native',
            'refresh_token': 'old-refresh',
          });
          expect(saved, isNull);
          refreshStarted.complete();
          await sendRefresh.future;
          await _reply(request, 200, {
            'data': {
              'access_token': 'new-access',
              'refresh_token': 'new-refresh',
            },
          });
          return;
        }

        expect(request.uri.path, '/api/v1/me');
        final authorization = request.headers.value('Authorization');
        if (authorization == 'Bearer old-access') {
          oldRequests++;
          if (oldRequests == 3) await sendLateExpiry.future;
          await _reply(request, 401, {
            'error': {'code': 'TOKEN_EXPIRED'},
          });
          expiredResponses++;
          if (expiredResponses == 2) firstExpiries.complete();
        } else {
          expect(authorization, 'Bearer new-access');
          expect(jsonDecode(saved!)['refresh_token'], 'new-refresh');
          final requestNumber = ++newRequests;
          await _reply(request, 200, {
            'data': {
              'user': {'username': 'root'},
            },
          });
          if (requestNumber == 2) firstRetries.complete();
        }
      });

      final account = MobileAccount();
      await account.remember(_session(server.port));
      expect((jsonDecode(saved!) as Map).containsKey('access_token'), isFalse);
      final reads = Future.wait(List.generate(3, (_) => account.readProfile()));
      await refreshStarted.future;
      await firstExpiries.future;
      sendRefresh.complete();
      await firstRetries.future;
      sendLateExpiry.complete();
      expect(await reads, ['root', 'root', 'root']);
      expect((oldRequests, refreshes, newRequests), (3, 1, 3));
    }, _LocalNetwork());
  });

  test(
    'a malformed rotation requires login and does not replay on restore',
    () async {
      await HttpOverrides.runWithHttpOverrides(() async {
        final server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
        addTearDown(() => server.close(force: true));
        var refreshes = 0;
        server.listen((request) async {
          expect(request.uri.path, '/api/v1/auth/refresh');
          await request.drain<void>();
          refreshes++;
          await _reply(request, 200, {
            'data': {'access_token': 'new-access'},
          });
        });

        await MobileAccount().remember(_session(server.port));
        final reopened = MobileAccount();
        await expectLater(
          reopened.restore(),
          throwsA(isA<ReauthenticationRequired>()),
        );
        expect(reopened.username, isNull);
        expect(saved, isNull);
        await MobileAccount().restore();
        expect(refreshes, 1);
      }, _LocalNetwork());
    },
  );
}

NativeSession _session(int port) => NativeSession(
  address: Uri.parse('http://127.0.0.1:$port'),
  username: 'root',
  accessToken: 'old-access',
  refreshToken: 'old-refresh',
);

Future<void> _reply(HttpRequest request, int status, Object body) async {
  request.response.statusCode = status;
  request.response.headers.contentType = ContentType.json;
  request.response.write(jsonEncode(body));
  await request.response.close();
}
