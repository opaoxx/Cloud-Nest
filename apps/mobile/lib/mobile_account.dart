import 'dart:convert';

import 'package:flutter/services.dart';

import 'native_login.dart';

const _credentials = MethodChannel('cloudnest/android_session');
typedef _SavedSession = ({Uri address, String username, String refreshToken});

class ReauthenticationRequired extends AccountFailure {
  const ReauthenticationRequired([super.message = '会话已失效，请重新登录。']);
}

class MobileAccount {
  NativeSession? _session;
  Future<NativeSession>? _refreshing;

  String? get username => _session?.username;

  Future<void> remember(NativeSession session) async {
    // 保存的是服务地址、账号和刷新凭证，访问令牌只留在内存。
    await _write(session);
    _session = session;
  }

  Future<void> restore() async {
    // 1. 重开应用时读密文记录，凭证仍在时通过后端轮换恢复。
    final _SavedSession? saved;
    try {
      saved = await _read();
    } on AccountFailure {
      throw const ReauthenticationRequired('无法读取保存的会话，请重新登录。');
    }
    if (saved != null) await _rotateOnce(saved);
  }

  Future<String> readProfile() async {
    var session = _session;
    if (session == null) throw const ReauthenticationRequired();
    try {
      // 1. 每个调用可以先独立读取。只有明确的 TOKEN_EXPIRED 才尝试轮换。
      try {
        final username = await readCurrentUsername(session);
        if (_session == null) throw const ReauthenticationRequired();
        return username;
      } on AccessTokenExpired {
        session = await _refreshFor(session);
      }

      // 2. 各调用用新访问令牌重试一次。重试再过期时交回界面。
      final username = await readCurrentUsername(session);
      if (_session == null) throw const ReauthenticationRequired();
      return username;
    } on AccessTokenExpired {
      throw const AccountFailure('访问令牌再次过期，请重新核对会话。');
    } on SessionRejected {
      // 3. 远端明确撤销了当前会话时，清除本机记录并回到登录入口。
      if (identical(_session, session) && _refreshing == null) {
        _session = null;
        await _write(null);
      }
      if (_session == null) throw const ReauthenticationRequired();
      throw const AccountFailure('会话已更新，请重新核对。');
    }
  }

  Future<NativeSession> _refreshFor(NativeSession previous) async {
    final current = _session;
    if (current == null) throw const ReauthenticationRequired();

    // 延迟到达的过期请求直接复用别人已保存的轮换结果。
    if (!identical(current, previous)) return current;
    return _rotateOnce((
      address: current.address,
      username: current.username,
      refreshToken: current.refreshToken,
    ));
  }

  Future<NativeSession> _rotateOnce(_SavedSession saved) {
    return _refreshing ??= _rotate(saved).whenComplete(() {
      _refreshing = null;
    });
  }

  Future<NativeSession> _rotate(_SavedSession saved) async {
    try {
      // 1. 先落盘空记录。遇到进程中断或回执丢失时，启动也不会重放旧凭证。
      await _write(null);

      // 2. 向服务轮换一次，将新凭证安全写入再替换内存会话。
      final next = await refreshAndroid(
        saved.address,
        saved.username,
        saved.refreshToken,
      );
      await remember(next);
      return next;
    } on SessionRejected {
      _session = null;
      throw const ReauthenticationRequired();
    } on AccountFailure {
      _session = null;
      throw const ReauthenticationRequired('无法确认会话轮换结果，请重新登录。');
    }
  }

  Future<_SavedSession?> _read() async {
    final String? text;
    try {
      text = await _credentials.invokeMethod<String>('read');
    } on MissingPluginException {
      return null;
    } on PlatformException {
      throw const AccountFailure('无法读取 Android 会话记录。');
    }
    if (text == null) return null;
    try {
      if (jsonDecode(text)
          case {
            'address': final String address,
            'username': final String username,
            'refresh_token': final String refreshToken,
          }
          when username.isNotEmpty && refreshToken.isNotEmpty) {
        return (
          address: parseServiceAddress(address),
          username: username,
          refreshToken: refreshToken,
        );
      }
    } on FormatException {
      throw const AccountFailure('本机会话记录损坏。');
    }
    throw const AccountFailure('本机会话记录不完整。');
  }

  Future<void> _write(NativeSession? session) async {
    try {
      if (session == null) {
        await _credentials.invokeMethod<void>('clear');
      } else {
        await _credentials.invokeMethod<void>(
          'write',
          jsonEncode({
            'address': session.address.toString(),
            'username': session.username,
            'refresh_token': session.refreshToken,
          }),
        );
      }
    } on PlatformException {
      throw const AccountFailure('无法安全更新本机会话，请重新登录。');
    } on MissingPluginException {
      throw const AccountFailure('当前设备无法安全保存会话。');
    }
  }
}
