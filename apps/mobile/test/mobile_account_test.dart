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
              'user': {'username': 'root', 'id': '1'},
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

  test('logout clears locally before an expired token is refreshed', () async {
    await HttpOverrides.runWithHttpOverrides(() async {
      final server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
      addTearDown(() => server.close(force: true));
      final requests = <String>[];
      server.listen((request) async {
        final authorization = request.headers.value('Authorization');
        final body = jsonDecode(await utf8.decoder.bind(request).join());
        expect(request.method, 'POST');
        expect(request.headers.value('X-CloudNest-Client'), 'native');
        expect(request.headers.value('Origin'), isNull);
        expect(saved, isNull);
        switch (request.uri.path) {
          case '/api/v1/auth/logout':
            expect(body, {'client_type': 'native'});
            if (authorization == 'Bearer old-access') {
              requests.add('expired logout');
              await _reply(request, 401, {
                'error': {'code': 'TOKEN_EXPIRED'},
              });
            } else {
              expect(authorization, 'Bearer new-access');
              requests.add('logout');
              await _reply(request, 200, {
                'data': {'revoked': true},
              });
            }
          case '/api/v1/auth/refresh':
            expect(authorization, isNull);
            expect(body, {
              'client_type': 'native',
              'refresh_token': 'old-refresh',
            });
            requests.add('refresh');
            await _reply(request, 200, {
              'data': {
                'access_token': 'new-access',
                'refresh_token': 'new-refresh',
              },
            });
          default:
            fail('Unexpected path ${request.uri.path}');
        }
      });

      final account = MobileAccount();
      await account.remember(_session(server.port));
      expect(account.owner, (
        server: 'http://127.0.0.1:${server.port}',
        userId: '1',
      ));
      expect(await account.logout(), '已退出账号，服务端会话已结束。');
      expect(account.owner, isNull);
      expect(saved, isNull);
      await MobileAccount().restore();
      expect(requests, ['expired logout', 'refresh', 'logout']);
    }, _LocalNetwork());
  });

  test(
    'logout retires pending account reads even when remote logout fails',
    () async {
      await HttpOverrides.runWithHttpOverrides(() async {
        final server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
        addTearDown(() => server.close(force: true));
        final readStarted = Completer<void>();
        final releaseRead = Completer<void>();
        server.listen((request) async {
          switch (request.uri.path) {
            case '/api/v1/me':
              readStarted.complete();
              await releaseRead.future;
              await _reply(request, 200, {
                'data': {
                  'user': {'username': 'root', 'id': '1'},
                },
              });
            case '/api/v1/auth/logout':
              await request.drain<void>();
              expect(saved, isNull);
              await _reply(request, 503, {});
            default:
              fail('Unexpected path ${request.uri.path}');
          }
        });

        final account = MobileAccount();
        await account.remember(_session(server.port));
        final read = expectLater(
          account.readProfile(),
          throwsA(isA<ReauthenticationRequired>()),
        );
        await readStarted.future;
        expect(await account.logout(), '已退出本机，未能确认服务端会话已撤销。');
        expect(saved, isNull);
        await account.remember(
          NativeSession(
            address: Uri.parse('http://127.0.0.1:${server.port}'),
            username: 'second',
            userId: '2',
            accessToken: 'second-access',
            refreshToken: 'second-refresh',
          ),
        );
        releaseRead.complete();
        await read;
        expect(account.owner?.userId, '2');
      }, _LocalNetwork());
    },
  );

  test(
    'restores a previous-format session by reading the server user ID',
    () async {
      await HttpOverrides.runWithHttpOverrides(() async {
        final server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
        addTearDown(() => server.close(force: true));
        server.listen((request) async {
          switch (request.uri.path) {
            case '/api/v1/auth/refresh':
              await request.drain<void>();
              await _reply(request, 200, {
                'data': {
                  'access_token': 'new-access',
                  'refresh_token': 'new-refresh',
                },
              });
            case '/api/v1/me':
              expect(
                request.headers.value('Authorization'),
                'Bearer new-access',
              );
              await _reply(request, 200, {
                'data': {
                  'user': {'username': 'root', 'id': '17'},
                },
              });
            default:
              fail('Unexpected path ${request.uri.path}');
          }
        });

        final address = 'http://127.0.0.1:${server.port}';
        saved = jsonEncode({
          'address': address,
          'username': 'root',
          'refresh_token': 'old-refresh',
        });
        final account = MobileAccount();
        await account.restore();
        expect(account.owner, (server: address, userId: '17'));
        expect(jsonDecode(saved!)['user_id'], '17');
      }, _LocalNetwork());
    },
  );
}

NativeSession _session(int port) => NativeSession(
  address: Uri.parse('http://127.0.0.1:$port'),
  username: 'root',
  userId: '1',
  accessToken: 'old-access',
  refreshToken: 'old-refresh',
);

Future<void> _reply(HttpRequest request, int status, Object body) async {
  request.response.statusCode = status;
  request.response.headers.contentType = ContentType.json;
  request.response.write(jsonEncode(body));
  await request.response.close();
}
