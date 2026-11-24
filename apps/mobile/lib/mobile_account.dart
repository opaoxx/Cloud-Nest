import 'dart:convert';
import 'dart:io';

import 'package:flutter/services.dart';

import 'account_owner.dart';
import 'mobile_tasks.dart';
import 'native_directory.dart';
import 'native_download.dart';
import 'native_login.dart';
import 'native_multipart_completion.dart';
import 'native_multipart_reconcile.dart';
import 'native_multipart_upload.dart';
import 'native_single_upload.dart';
import 'native_upload_creation.dart';

export 'account_owner.dart' show AccountOwner, documentScopeFor;

const _credentials = MethodChannel('cloudnest/android_session');
const _documents = MethodChannel('cloudnest/android_document');
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

  Future<String?> downloadFile(DirectoryEntry entry) async {
    final currentOwner = owner;
    if (currentOwner == null || _signingOut) {
      throw const ReauthenticationRequired();
    }
    final epoch = _sessionEpoch;

    // 1. API 访问按当前账号授权。签名交给 Android，正文请求独立发出。
    final grant = await _readForAccount(
      (session) => createDownloadGrant(session, entry),
    );
    _checkSession(epoch);
    final actualName = await saveGrantedDownload(grant, entry.name);
    if (actualName == null) return null;

    // 2. 只有系统保存成功才记任务。记录失败时仍告知用户实际保存名称。
    _checkSession(epoch);
    try {
      await recordSavedDownload(currentOwner, actualName, grant);
    } on TaskStoreFailure {
      throw AccountFailure('已保存「$actualName」，本机任务记录写入失败。');
    }
    return actualName;
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

  Future<String> uploadMultipart(
    PreparedUploadFile source,
    String parentId, {
    required void Function(MultipartCreation plan, int confirmed) onProgress,
  }) async {
    final currentOwner = owner;
    if (currentOwner == null || _signingOut) {
      throw const ReauthenticationRequired();
    }
    final epoch = _sessionEpoch;
    if (source.sizeBytes <= maxSingleUploadBytes ||
        source.sizeBytes > maxUploadProtocolBytes) {
      throw const AccountFailure('文件大小超出分片创建支持的范围。');
    }

    try {
      // 1. 校验源文件，读取原任务的服务端分片清单。
      final intent = await reserveMultipartIntent(
        currentOwner,
        parentId,
        source,
      );
      _checkSession(epoch);
      final plan =
          intent.creation ??
          await _readForAccount<MultipartCreation>(
            (session) =>
                createMultipartUpload(session, intent.id, parentId, source),
          );
      if (intent.creation == null) {
        await saveMultipartCheckpoint(currentOwner, intent.id, plan);
      }
      _checkSession(epoch);
      Future<MultipartTask> queryTask() => _readForAccount(
        (session) => readMultipartTask(session, plan, parentId),
      );
      final waiting = Stopwatch()..start();
      var task = await queryTask();
      while (task.state == 'CREATING') {
        if (waiting.elapsed >= const Duration(seconds: 10)) {
          throw const AccountFailure('服务端仍在初始化任务，请稍后重试。');
        }
        await Future<void>.delayed(const Duration(milliseconds: 450));
        _checkSession(epoch);
        task = await queryTask();
      }
      if (task.state != 'UPLOADING') {
        return await _finishMultipart(
          currentOwner,
          intent.id,
          parentId,
          plan,
          const {},
          epoch,
          initialTask: task,
        );
      }
      if (!task.expiresAt.isAfter(DateTime.now())) {
        throw const AccountFailure('原分片任务的有效期已结束。');
      }

      final scope = documentScopeFor(currentOwner);
      final digests = await inspectMultipartSource(scope, source, plan);
      _checkSession(epoch);
      final remote = await _readForAccount(
        (session) => reconcileMultipartReceipts(session, plan, digests),
      );

      // 2. 按长度和摘要核对分片，在一个事务里更新本机回执。
      await saveReconciledMultipartParts(
        currentOwner,
        intent.id,
        plan.taskId,
        remote.values,
      );
      final saved = await readMultipartReceipts(currentOwner, intent.id, plan);
      _checkSession(epoch);
      var confirmed = saved.length;
      onProgress(plan, confirmed);
      for (var number = 1; number <= plan.partCount; number++) {
        if (saved.containsKey(number)) continue;
        _checkSession(epoch);
        final part = (
          number: number,
          offsetBytes: plan.offsetOf(number),
          sizeBytes: plan.sizeOf(number),
          sha256: digests[number - 1],
        );
        try {
          // 3. 补传缺片。签名明确过期时，核对原任务后重签一次。
          final receipt = await _sendMultipartWithRenewal(
            scope,
            source,
            part,
            plan,
            parentId,
          );
          await saveMultipartPart(
            currentOwner,
            intent.id,
            plan.taskId,
            receipt,
          );
          _checkSession(epoch);
          saved[number] = receipt;
          onProgress(plan, ++confirmed);
        } on ReauthenticationRequired {
          rethrow;
        } on AccountFailure catch (error) {
          throw AccountFailure(
            '第 $number 片未确认。已记录 $confirmed/${plan.partCount} 片。\n'
            '${error.message}\n恢复网络后可再次点击上传继续。',
          );
        }
      }
      return await _finishMultipart(
        currentOwner,
        intent.id,
        parentId,
        plan,
        saved,
        epoch,
      );
    } on TaskStoreFailure {
      throw const AccountFailure('无法读写本机上传记录，请重试。');
    }
  }

  Future<MultipartReceipt> _sendMultipartWithRenewal(
    String scope,
    PreparedUploadFile source,
    MultipartPart part,
    MultipartCreation plan,
    String parentId,
  ) async {
    final grant = await _readForAccount(
      (session) => requestMultipartPart(session, plan, part),
    );
    try {
      return await sendMultipartPart(scope, source, part, grant);
    } on MultipartSignatureExpired {
      final renewed = await _readForAccount((session) async {
        if (!await multipartReady(session, plan, parentId)) {
          throw const AccountFailure('原任务暂未准备好，请重试。');
        }
        return renewMultipartPart(session, plan, part, grant);
      });
      return sendMultipartPart(scope, source, part, renewed);
    }
  }

  Future<String> _finishMultipart(
    AccountOwner owner,
    String localId,
    String parentId,
    MultipartCreation plan,
    Map<int, MultipartReceipt> receipts,
    int epoch, {
    MultipartTask? initialTask,
  }) async {
    Future<MultipartTask> readTask() {
      _checkSession(epoch);
      return _readForAccount(
        (session) => readMultipartTask(session, plan, parentId),
      );
    }

    // 1. 查询原任务，按片号核对已保存的完整回执。
    var task = initialTask ?? await readTask();
    if (task.state == 'UPLOADING') {
      if (!task.expiresAt.isAfter(DateTime.now())) {
        throw const AccountFailure('原分片任务的有效期已结束。');
      }
      try {
        // 2. 提交合并请求，结果未确认时查回同一任务。
        task = await _readForAccount(
          (session) =>
              requestMultipartCompletion(session, plan, parentId, receipts),
        );
      } on ReauthenticationRequired {
        rethrow;
      } on AccountFailure {
        task = await readTask();
        if (task.state == 'UPLOADING') rethrow;
      }
    }

    // 3. 保存发布条目或失败原因，再更新页面。
    final waiting = Stopwatch()..start();
    while (true) {
      _checkSession(epoch);
      if (task.entryId case final String entryId) {
        await saveMultipartOutcome(owner, localId, plan.taskId, task);
        _checkSession(epoch);
        return entryId;
      }
      if (const {'FAILED', 'EXPIRED', 'CANCELLED'}.contains(task.state)) {
        await saveMultipartOutcome(owner, localId, plan.taskId, task);
        _checkSession(epoch);
        final label = switch (task.state) {
          'EXPIRED' => '原分片任务已过期',
          'CANCELLED' => '原分片任务已取消',
          _ => multipartFailureLabel(task.errorCode),
        };
        throw AccountFailure('$label。文件未发布。');
      }
      if (!const {
            'COMPLETING',
            'VERIFYING',
            'CANCELLING',
            'EXPIRING',
          }.contains(task.state) ||
          waiting.elapsed >= const Duration(seconds: 30)) {
        throw const AccountFailure('发布结果尚未确认。重试时会查询原任务。');
      }
      await Future<void>.delayed(const Duration(milliseconds: 450));
      task = await readTask();
    }
  }

  Future<String> uploadSingle(
    PreparedUploadFile source,
    String parentId,
  ) async {
    final currentOwner = owner;
    if (currentOwner == null || _signingOut) {
      throw const ReauthenticationRequired();
    }
    final epoch = _sessionEpoch;
    if (source.sizeBytes < 0 || source.sizeBytes > maxSingleUploadBytes) {
      throw const AccountFailure('小文件上传上限为 16 MiB。');
    }

    File? body;
    Future<File> openBody() async {
      _checkSession(epoch);
      return body ??= await _snapshot(currentOwner, source);
    }

    Future<T> remote<T>(Future<T> Function(NativeSession) call) {
      _checkSession(epoch);
      return _readForAccount(call);
    }

    try {
      // 1. 保存或恢复同一声明。首次创建前先复核正文。
      final intent = await reserveSingleIntent(currentOwner, parentId, source);
      final String taskId;
      if (intent.remoteId case final String existing) {
        taskId = existing;
      } else {
        await openBody();
        taskId = await remote(
          (session) => createSingleUpload(session, intent.id, parentId, source),
        );
        await saveSingleCheckpoint(currentOwner, intent.id, taskId, 'PENDING');
      }

      Future<SingleUploadTask> readTask() => remote(
        (session) =>
            readSingleUploadTask(session, taskId, parentId, source.sizeBytes),
      );

      // 2. 先对账再决定是否发送，避免重发已经收到的正文。
      var task = await readTask();
      if (task.state == 'UPLOADING') {
        final file = await openBody();
        try {
          await remote(
            (session) =>
                sendSingleUploadBody(session, taskId, file, source.sizeBytes),
          );
        } on AccountFailure {
          task = await readTask();
          if (task.state == 'UPLOADING') rethrow;
        }
      }

      // 3. 返回已发布的条目。未确认的进度保留原任务和原创建键。
      final waited = Stopwatch()..start();
      while (true) {
        if (task.entryId case final String entryId) {
          await saveSingleCheckpoint(
            currentOwner,
            intent.id,
            taskId,
            'COMPLETED',
            entryId: entryId,
          );
          return entryId;
        }
        if (const {'FAILED', 'EXPIRED', 'CANCELLED'}.contains(task.state)) {
          await saveSingleCheckpoint(currentOwner, intent.id, taskId, 'FAILED');
          throw AccountFailure(switch (task.errorCode) {
            'CHECKSUM_MISMATCH' ||
            'BODY_SIZE_MISMATCH' => '服务端校验发现正文与原文件不一致，请重新选择文件。',
            _ => '上传任务已结束，文件未发布。请重试。',
          });
        }
        if (waited.elapsed >= const Duration(seconds: 30)) {
          throw const AccountFailure('服务端仍在处理，重试会先查询原任务。');
        }
        await Future<void>.delayed(const Duration(milliseconds: 450));
        task = await readTask();
      }
    } on TaskStoreFailure {
      throw const AccountFailure('无法保存本机上传记录，请重试。');
    } finally {
      final snapshot = body;
      if (snapshot != null) {
        try {
          await snapshot.delete();
        } on FileSystemException {
          // 下次启动会回收上一进程留下的临时正文。
        }
      }
    }
  }

  Future<File> _snapshot(AccountOwner owner, PreparedUploadFile source) async {
    try {
      final result = await _documents.invokeMapMethod<String, Object?>(
        'snapshotForSingle',
        {
          'scope': documentScopeFor(owner),
          'name': source.name,
          'sizeBytes': source.sizeBytes,
          'sha256': source.sha256,
        },
      );
      if (result case {'path': final String path} when path.startsWith('/')) {
        return File(path);
      }
    } on PlatformException catch (error) {
      throw AccountFailure(switch (error.code) {
        'document_staging_space' => '手机可用空间不足，释放空间后请重试上传。',
        _ => '无法按原摘要读取文件，请返回首页重新选择。',
      });
    } on MissingPluginException {
      throw const AccountFailure('当前设备无法打开已准备的文件。');
    }
    throw const AccountFailure('无法打开已准备的文件，请重新选择。');
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
