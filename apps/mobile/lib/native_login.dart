import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'dart:typed_data' show BytesBuilder;

import 'package:flutter/foundation.dart';
import 'package:flutter/services.dart';

const _installation = MethodChannel('cloudnest/android_installation');
const _deadline = Duration(seconds: 10);
final _usernamePattern = RegExp(r'^[a-z0-9_]{1,64}$');
final _serverIdPattern = RegExp(r'^[1-9][0-9]{0,19}$');
final _maxServerId = BigInt.parse('18446744073709551615');
typedef NativeTokens = ({String accessToken, String refreshToken});
typedef NativeFileBody = ({File file, int sizeBytes});
typedef CurrentProfile = ({String username, String userId, String rootEntryId});

class NativeSession {
  const NativeSession({
    required this.address,
    required this.username,
    required this.userId,
    required this.accessToken,
    required this.refreshToken,
  });

  final Uri address;
  final String username;
  final String userId;
  final String accessToken;
  final String refreshToken;
}

class AccountFailure implements Exception {
  const AccountFailure(this.message);

  final String message;
}

class AccessTokenExpired implements Exception {
  const AccessTokenExpired();
}

class SessionRejected implements Exception {
  const SessionRejected();
}

Future<NativeSession> loginAndroid({
  required String address,
  required String username,
  required String password,
}) async {
  // 1. 校验服务根地址和登录字段，再向 Android 读取本次安装的标识。
  final base = parseServiceAddress(address);
  if (!_usernamePattern.hasMatch(username)) {
    throw const AccountFailure('用户名须为 1～64 位小写字母、数字或下划线。');
  }
  if (password.isEmpty || utf8.encode(password).length > 72) {
    throw const AccountFailure('密码长度须为 1～72 个 UTF-8 字节。');
  }

  final String? installationId;
  try {
    installationId = await _installation.invokeMethod<String>(
      'getInstallationId',
    );
  } on PlatformException {
    throw const AccountFailure('无法读取本机安装标识，请重试。');
  } on MissingPluginException {
    throw const AccountFailure('当前设备无法提供 Android 安装标识。');
  }
  if (installationId == null || installationId.isEmpty) {
    throw const AccountFailure('无法读取本机安装标识，请重试。');
  }

  // 2. 通过原生契约提交密码原文。跳转、超时或不完整的返回都不能建立会话。
  final response = await _request(
    base,
    '/api/v1/auth/login',
    payload: {
      'username': username,
      'password': password,
      'installation_id': installationId,
      'device_name': 'CloudNest Android',
      'platform': 'ANDROID',
      'client_type': 'native',
    },
  );
  switch (response.status) {
    case HttpStatus.ok:
      break;
    case HttpStatus.unauthorized:
      throw const AccountFailure('用户名或密码错误，请重试。');
    case HttpStatus.badRequest:
      throw const AccountFailure('登录参数有误，请检查输入。');
    case HttpStatus.forbidden:
      throw const AccountFailure('服务拒绝了本次登录，请检查账号状态。');
    case HttpStatus.tooManyRequests:
      throw const AccountFailure('登录请求过于频繁，请稍后重试。');
    default:
      throw AccountFailure('CloudNest 登录失败（HTTP ${response.status}）。');
  }

  // 3. 取得服务端的用户 ID，用它归属本机文件记录和后续任务。
  if (response.body
      case {
        'data': {
          'user': {
            'username': final String returnedUsername,
            'id': final String userId,
          },
        },
      }
      when returnedUsername == username && isCloudNestId(userId)) {
    final tokens = _readTokens(response.body);
    return NativeSession(
      address: base,
      username: returnedUsername,
      userId: userId,
      accessToken: tokens.accessToken,
      refreshToken: tokens.refreshToken,
    );
  }
  throw const AccountFailure('收到的登录响应与当前账号不一致。');
}

Uri parseServiceAddress(String address) {
  final base = Uri.tryParse(address.trim());
  if (base == null ||
      (base.scheme != 'https' && base.scheme != 'http') ||
      base.host.isEmpty ||
      base.authority.contains('@') ||
      (base.path.isNotEmpty && base.path != '/') ||
      base.hasQuery ||
      base.hasFragment ||
      (base.hasPort && (base.port < 1 || base.port > 65535))) {
    throw const AccountFailure('请输入服务根地址，例如 https://cloud.example.com');
  }
  if (base.scheme == 'http' && !kDebugMode) {
    throw const AccountFailure('请使用 HTTPS 服务地址。');
  }
  return base;
}

bool isCloudNestId(String value) =>
    _serverIdPattern.hasMatch(value) && BigInt.parse(value) <= _maxServerId;

Future<NativeTokens> refreshAndroid(Uri address, String refreshToken) async {
  // 1. 原生刷新请求通过 JSON 提交当前凭证，不发送访问令牌。
  final result = await _request(
    address,
    '/api/v1/auth/refresh',
    payload: {'client_type': 'native', 'refresh_token': refreshToken},
  );
  if (result.status == HttpStatus.unauthorized ||
      result.status == HttpStatus.forbidden) {
    throw const SessionRejected();
  }
  if (result.status != HttpStatus.ok) {
    throw const AccountFailure('服务暂时无法确认刷新结果。');
  }

  // 2. 新响应必须交付新刷新凭证，旧凭证不能再次写回本机。
  final next = _readTokens(result.body);
  if (next.refreshToken == refreshToken) throw const SessionRejected();
  return next;
}

Future<void> logoutAndroid(Uri address, String accessToken) async {
  final result = await _request(
    address,
    '/api/v1/auth/logout',
    payload: {'client_type': 'native'},
    token: accessToken,
  );
  if (result case (status: HttpStatus.ok, body: {'data': {'revoked': true}})) {
    return;
  }
  try {
    _checkAuthorization(result);
  } on SessionRejected {
    return;
  }
  throw const AccountFailure('服务端未确认撤销会话。');
}

Future<CurrentProfile> readCurrentProfile(
  Uri address,
  String accessToken,
) async {
  final result = await requestAuthorizedJson(
    address,
    accessToken,
    '/api/v1/me',
  );
  if (result
      case (
        status: HttpStatus.ok,
        body: {
          'data': {
            'root_entry_id': final String rootEntryId,
            'user': {
              'username': final String username,
              'id': final String userId,
            },
          },
        },
      )
      when username.isNotEmpty &&
          isCloudNestId(userId) &&
          isCloudNestId(rootEntryId)) {
    return (username: username, userId: userId, rootEntryId: rootEntryId);
  }
  throw const AccountFailure('无法核对账号资料，请检查服务后重试。');
}

Future<({int status, Object? body})> requestAuthorizedJson(
  Uri address,
  String accessToken,
  String path, {
  Map<String, String>? query,
  Map<String, Object>? payload,
  String? creationKey,
  NativeFileBody? fileBody,
  bool emptyPost = false,
  int successStatus = HttpStatus.ok,
  Set<int> additionalJsonStatuses = const {},
  int maxResponseBytes = 64 * 1024,
}) async {
  final result = await _request(
    address,
    path,
    token: accessToken,
    query: query,
    payload: payload,
    creationKey: creationKey,
    fileBody: fileBody,
    emptyPost: emptyPost,
    successStatus: successStatus,
    additionalJsonStatuses: additionalJsonStatuses,
    maxResponseBytes: maxResponseBytes,
  );
  _checkAuthorization(result);
  return result;
}

void _checkAuthorization(({int status, Object? body}) result) {
  if (result case (
    status: HttpStatus.unauthorized,
    body: {'error': {'code': final String code}},
  )) {
    if (code == 'TOKEN_EXPIRED') throw const AccessTokenExpired();
    if (code == 'SESSION_REVOKED') throw const SessionRejected();
  }
}

NativeTokens _readTokens(Object? body) {
  if (body
      case {
        'data': {
          'access_token': final String accessToken,
          'refresh_token': final String refreshToken,
        },
      }
      when accessToken.isNotEmpty && refreshToken.isNotEmpty) {
    return (accessToken: accessToken, refreshToken: refreshToken);
  }
  throw const AccountFailure('收到的会话凭证不完整。');
}

Future<({int status, Object? body})> _request(
  Uri base,
  String path, {
  Map<String, Object>? payload,
  String? token,
  Map<String, String>? query,
  String? creationKey,
  NativeFileBody? fileBody,
  bool emptyPost = false,
  int successStatus = HttpStatus.ok,
  Set<int> additionalJsonStatuses = const {},
  int maxResponseBytes = 64 * 1024,
}) async {
  final client = HttpClient()..connectionTimeout = _deadline;
  try {
    return await _send(
      client,
      base.replace(path: path, queryParameters: query),
      payload: payload,
      token: token,
      creationKey: creationKey,
      fileBody: fileBody,
      emptyPost: emptyPost,
      successStatus: successStatus,
      additionalJsonStatuses: additionalJsonStatuses,
      maxResponseBytes: maxResponseBytes,
    ).timeout(fileBody == null ? _deadline : const Duration(seconds: 310));
  } on TimeoutException {
    throw const AccountFailure('请求超时，请检查网络后重试。');
  } on IOException {
    throw const AccountFailure('无法连接服务，请检查地址、网络和服务状态。');
  } on FormatException {
    throw const AccountFailure('收到的响应无效，请检查 CloudNest 服务。');
  } finally {
    client.close(force: true);
  }
}

Future<({int status, Object? body})> _send(
  HttpClient client,
  Uri uri, {
  Map<String, Object>? payload,
  String? token,
  String? creationKey,
  NativeFileBody? fileBody,
  required bool emptyPost,
  required int successStatus,
  required Set<int> additionalJsonStatuses,
  required int maxResponseBytes,
}) async {
  final method = fileBody != null
      ? 'PUT'
      : (payload != null || emptyPost ? 'POST' : 'GET');
  final request = await client.openUrl(method, uri);
  request.followRedirects = false;
  if (emptyPost) request.contentLength = 0;
  if (token != null) {
    request.headers.set(HttpHeaders.authorizationHeader, 'Bearer $token');
  }
  if (creationKey != null) request.headers.set('Idempotency-Key', creationKey);
  if (payload != null) {
    request.headers.contentType = ContentType.json;
    if (uri.path.startsWith('/api/v1/auth/')) {
      request.headers.set('X-CloudNest-Client', 'native');
    }
    request.add(utf8.encode(jsonEncode(payload)));
  }
  if (fileBody != null) {
    request.headers.contentType = ContentType.binary;
    request.contentLength = fileBody.sizeBytes;
    if (fileBody.sizeBytes > 0) {
      await request.addStream(fileBody.file.openRead());
    }
  }
  final response = await request.close();
  final status = response.statusCode;
  if (status != successStatus &&
      !additionalJsonStatuses.contains(status) &&
      (token == null || status != HttpStatus.unauthorized) &&
      (creationKey == null || status != HttpStatus.conflict)) {
    return (status: status, body: null);
  }

  final body = BytesBuilder(copy: false);
  await for (final chunk in response) {
    if (body.length + chunk.length > maxResponseBytes) {
      throw const FormatException('Response is too large');
    }
    body.add(chunk);
  }
  return (status: status, body: jsonDecode(utf8.decode(body.takeBytes())));
}
