import 'dart:convert';

import 'package:flutter/services.dart';

import 'native_directory.dart';
import 'native_login.dart';

const _credentials = MethodChannel('cloudnest/android_session');
typedef AccountOwner = ({String server, String userId});
typedef _SavedSession = ({
  Uri address,
  String username,
  String? userId,
  String refreshToken,
});

class ReauthenticationRequired extends AccountFailure {
  const ReauthenticationRequired([super.message = '会话已失效，请重新登录。']);
}

class MobileAccount {
  NativeSession? _session;
  Future<NativeSession>? _refreshing;
  bool _signingOut = false;
  int _sessionEpoch = 0;

  String? get username => _session?.username;

  AccountOwner? get owner {
    final session = _session;
    return session == null
        ? null
        : (server: session.address.origin, userId: session.userId);
  }

  Future<void> remember(NativeSession session) async {
    if (_signingOut || _refreshing != null) {
      throw const AccountFailure('会话正在更新，请稍后重试。');
    }
    // 保存的是服务地址、账号和刷新凭证，访问令牌只留在内存。
    await _write(session);
    _session = session;
    _sessionEpoch++;
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

  Future<String> readProfile() async => (await currentProfile()).username;

  Future<CurrentProfile> currentProfile() => _readForAccount((session) async {
    final profile = await readCurrentProfile(
      session.address,
      session.accessToken,
    );
    if (profile.userId != session.userId ||
        profile.username != session.username) {
      throw const AccountFailure('服务返回的资料与当前账号不一致。');
    }
    return profile;
  });

  Future<DirectoryListing> listDirectory({
    required String rootId,
    required String parentId,
    String? cursor,
  }) => _readForAccount(
    (session) => readDirectoryListing(
      session,
      rootId: rootId,
      parentId: parentId,
      cursor: cursor,
    ),
  );

  Future<Result> _readForAccount<Result>(
    Future<Result> Function(NativeSession) read,
  ) async {
    var session = _session;
    if (session == null || _signingOut) throw const ReauthenticationRequired();
    final epoch = _sessionEpoch;
    try {
      // 1. 每个调用可以先独立读取。只有明确的 TOKEN_EXPIRED 才尝试轮换。
      Result result;
      try {
        result = await read(session);
      } on AccessTokenExpired {
        _checkSession(epoch);
        session = await _refreshFor(session);
        // 2. 各调用用新访问令牌重试一次。
        _checkSession(epoch);
        result = await read(session);
      }

      _checkSession(epoch);
      return result;
    } on AccessTokenExpired {
      throw const AccountFailure('访问令牌再次过期，请重试。');
    } on SessionRejected {
      // 3. 远端明确撤销了当前会话时，清除本机记录并回到登录入口。
      _checkSession(epoch);
      if (identical(_session, session) && _refreshing == null) {
        _session = null;
        _sessionEpoch++;
        await _write(null);
      }
      if (_session == null) throw const ReauthenticationRequired();
      throw const AccountFailure('会话已更新，请重试。');
    }
  }

  Future<String> logout() async {
    final current = _session;
    if (current == null) return '当前已退出账号。';
    if (_signingOut) throw const AccountFailure('正在退出账号，请稍候。');
    _signingOut = true;
    _sessionEpoch++;
    try {
      // 1. 停止旧会话交付结果，等待已发出的刷新落盘或结束，再清除本机凭证。
      final pending = _refreshing;
      var session = current;
      var canRefresh = true;
      if (pending != null) {
        try {
          session = await pending;
        } on AccountFailure {
          // 已发出的刷新可能被服务端接受。退出时不得重放这个刷新凭证。
          canRefresh = false;
        }
      }
      try {
        await _write(null);
      } on AccountFailure {
        throw const AccountFailure('无法清除本机凭证，请重试退出。');
      }
      _session = null;

      // 2. 访问令牌明确过期时，只为此次撤销刷新一次，令牌不写回本机。
      final revoked = await _revokeRemote(session, canRefresh: canRefresh);
      return revoked ? '已退出账号，服务端会话已结束。' : '已退出本机，未能确认服务端会话已撤销。';
    } finally {
      _signingOut = false;
    }
  }

  Future<bool> _revokeRemote(
    NativeSession session, {
    required bool canRefresh,
  }) async {
    try {
      try {
        await logoutAndroid(session.address, session.accessToken);
      } on AccessTokenExpired {
        if (!canRefresh) return false;
        final tokens = await refreshAndroid(
          session.address,
          session.refreshToken,
        );
        await logoutAndroid(session.address, tokens.accessToken);
      }
      return true;
    } on AccessTokenExpired {
      return false;
    } on AccountFailure {
      return false;
    } on SessionRejected {
      return false;
    }
  }

  void _checkSession(int epoch) {
    if (_signingOut || _session == null || epoch != _sessionEpoch) {
      throw const ReauthenticationRequired();
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
      userId: current.userId,
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
      final tokens = await refreshAndroid(saved.address, saved.refreshToken);
      final userId =
          saved.userId ?? await _upgradeUserId(saved, tokens.accessToken);
      final next = NativeSession(
        address: saved.address,
        username: saved.username,
        userId: userId,
        accessToken: tokens.accessToken,
        refreshToken: tokens.refreshToken,
      );
      await _write(next);
      _session = next;
      return next;
    } on SessionRejected {
      _session = null;
      throw const ReauthenticationRequired();
    } on AccessTokenExpired {
      _session = null;
      throw const ReauthenticationRequired();
    } on AccountFailure {
      _session = null;
      throw const ReauthenticationRequired('无法确认会话轮换结果，请重新登录。');
    }
  }

  Future<String> _upgradeUserId(_SavedSession saved, String accessToken) async {
    // 第 42 步的凭证只有账号名，首次升级时从本人资料补齐稳定用户 ID。
    final profile = await readCurrentProfile(saved.address, accessToken);
    if (profile.username != saved.username) {
      throw const AccountFailure('本机账号与服务返回的资料不一致。');
    }
    return profile.userId;
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
      final body = jsonDecode(text);
      if (body
          case {
            'address': final String address,
            'username': final String username,
            'refresh_token': final String refreshToken,
          }
          when username.isNotEmpty && refreshToken.isNotEmpty) {
        final userId = body['user_id'];
        if (userId != null && (userId is! String || !isCloudNestId(userId))) {
          throw const AccountFailure('本机账号标识无效。');
        }
        return (
          address: parseServiceAddress(address),
          username: username,
          userId: userId as String?,
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
            'user_id': session.userId,
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
