import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'dart:typed_data' show BytesBuilder;

import 'package:flutter/foundation.dart';
import 'package:flutter/services.dart';

const _installation = MethodChannel('cloudnest/android_installation');
const _deadline = Duration(seconds: 10);
final _usernamePattern = RegExp(r'^[a-z0-9_]{1,64}$');

class NativeSession {
  const NativeSession({
    required this.address,
    required this.username,
    required this.accessToken,
    required this.refreshToken,
  });

  final Uri address;
  final String username;
  final String accessToken;
  final String refreshToken;
}

class LoginFailure implements Exception {
  const LoginFailure(this.message);

  final String message;
}

Future<NativeSession> loginAndroid({
  required String address,
  required String username,
  required String password,
}) async {
  // 1. 校验服务根地址和登录字段，再向 Android 读取本次安装的标识。
  final base = Uri.tryParse(address.trim());
  if (base == null ||
      (base.scheme != 'https' && base.scheme != 'http') ||
      base.host.isEmpty ||
      base.authority.contains('@') ||
      (base.path.isNotEmpty && base.path != '/') ||
      base.hasQuery ||
      base.hasFragment ||
      (base.hasPort && (base.port < 1 || base.port > 65535))) {
    throw const LoginFailure('请输入服务根地址，例如 https://cloud.example.com');
  }
  if (base.scheme == 'http' && !kDebugMode) {
    throw const LoginFailure('请使用 HTTPS 服务地址。');
  }
  if (!_usernamePattern.hasMatch(username)) {
    throw const LoginFailure('用户名须为 1～64 位小写字母、数字或下划线。');
  }
  if (password.isEmpty || utf8.encode(password).length > 72) {
    throw const LoginFailure('密码长度须为 1～72 个 UTF-8 字节。');
  }

  final String? installationId;
  try {
    installationId = await _installation.invokeMethod<String>(
      'getInstallationId',
    );
  } on PlatformException {
    throw const LoginFailure('无法读取本机安装标识，请重试。');
  } on MissingPluginException {
    throw const LoginFailure('当前设备无法提供 Android 安装标识。');
  }
  if (installationId == null || installationId.isEmpty) {
    throw const LoginFailure('无法读取本机安装标识，请重试。');
  }

  // 2. 通过原生契约提交密码原文。跳转、超时或不完整的返回都不能建立会话。
  final payload = {
    'username': username,
    'password': password,
    'installation_id': installationId,
    'device_name': 'CloudNest Android',
    'platform': 'ANDROID',
    'client_type': 'native',
  };
  final client = HttpClient()..connectionTimeout = _deadline;
  try {
    return await _postLogin(client, base, payload).timeout(_deadline);
  } on TimeoutException {
    throw const LoginFailure('登录请求超时，请检查网络后重试。');
  } on IOException {
    throw const LoginFailure('无法连接服务，请检查地址、网络和服务状态。');
  } on FormatException {
    throw const LoginFailure('收到的登录响应无效，请检查 CloudNest 服务。');
  } finally {
    client.close(force: true);
  }
}

Future<NativeSession> _postLogin(
  HttpClient client,
  Uri base,
  Map<String, String> payload,
) async {
  final request = await client.postUrl(base.resolve('/api/v1/auth/login'));
  request.followRedirects = false;
  request.headers.contentType = ContentType.json;
  request.headers.set('X-CloudNest-Client', 'native');
  request.add(utf8.encode(jsonEncode(payload)));
  final response = await request.close();
  switch (response.statusCode) {
    case HttpStatus.ok:
      break;
    case HttpStatus.unauthorized:
      throw const LoginFailure('用户名或密码错误，请重试。');
    case HttpStatus.badRequest:
      throw const LoginFailure('登录参数有误，请检查输入。');
    case HttpStatus.forbidden:
      throw const LoginFailure('服务拒绝了本次登录，请检查账号状态。');
    case HttpStatus.tooManyRequests:
      throw const LoginFailure('登录请求过于频繁，请稍后重试。');
    default:
      throw LoginFailure('CloudNest 登录失败（HTTP ${response.statusCode}）。');
  }

  // 3. 只接受包含账号及两种原生凭证的成功响应。
  const maxResponseBytes = 64 * 1024;
  final body = BytesBuilder(copy: false);
  await for (final chunk in response) {
    if (body.length + chunk.length > maxResponseBytes) {
      throw const FormatException('Login response is too large');
    }
    body.add(chunk);
  }
  final decoded = jsonDecode(utf8.decode(body.takeBytes()));
  if (decoded
      case {
        'data': {
          'user': {'username': final String username},
          'access_token': final String accessToken,
          'refresh_token': final String refreshToken,
        },
      }
      when username.isNotEmpty &&
          accessToken.isNotEmpty &&
          refreshToken.isNotEmpty) {
    return NativeSession(
      address: base,
      username: username,
      accessToken: accessToken,
      refreshToken: refreshToken,
    );
  }
  throw const FormatException('Missing native session fields');
}
