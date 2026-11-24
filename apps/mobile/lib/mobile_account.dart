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
import 'native_upload_reuse.dart';
import 'native_upload_status.dart';
import 'transfer_pause.dart';

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

  Future<DirectoryEntry> editFile(
    DirectoryEntry entry,
    String parentId, {
    String? name,
  }) {
    final key = newFileActionKey();
    return _readForAccount(
      (session) =>
          changeDirectoryFile(session, entry, parentId, key, name: name),
    );
  }

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

  Future<String?> downloadFile(
    DirectoryEntry entry, {
    TransferPause? pause,
  }) async {
    final currentOwner = owner;
    if (currentOwner == null || _signingOut) {
      throw const ReauthenticationRequired();
    }
    final epoch = _sessionEpoch;

    try {
      final grant = await _readForAccount(
        (session) => createDownloadGrant(session, entry),
      );
      _checkSession(epoch);
      final id = await reserveDownload(currentOwner, entry, grant);
      return await _transferDownload(
        currentOwner,
        id,
        entry,
        epoch,
        pause,
        () async => grant,
      );
    } on TaskStoreFailure {
      throw const AccountFailure('无法读写本机下载记录，请重试。');
    }
  }

  Future<String?> resumeDownload(
    AccountOwner taskOwner,
    String id, {
    TransferPause? pause,
  }) async {
    if (owner != taskOwner || _signingOut) {
      throw const ReauthenticationRequired();
    }
    final epoch = _sessionEpoch;
    try {
      final task = await readPendingDownload(taskOwner, id);
      _checkSession(epoch);
      if (task == null) throw const AccountFailure('原下载已经结束或不可继续。');
      return await _transferDownload(
        taskOwner,
        id,
        task.entry,
        epoch,
        pause,
        () async {
          // 继续只允许为原对象授权。新修订需用户回云盘另行选择。
          final grant = await _readForAccount(
            (session) => createDownloadGrant(session, task.entry),
          );
          if (grant.objectId != task.objectId || grant.sha256 != task.sha256) {
            throw const TransferTargetUnavailable('文件对象已变化，请回到云盘重新选择。');
          }
          return grant;
        },
      );
    } on TaskStoreFailure {
      throw const AccountFailure('无法读写本机下载记录，请重试。');
    }
  }

  Future<String?> _transferDownload(
    AccountOwner owner,
    String id,
    DirectoryEntry entry,
    int epoch,
    TransferPause? pause,
    Future<DownloadGrant> Function() authorize,
  ) async {
    final actualName = await _withPauseRecord(owner, id, () async {
      _checkSession(epoch);
      pause?.check();
      final grant = await authorize();
      _checkSession(epoch);
      pause?.check();
      await setTransferPauseReason(owner, id, null);
      _checkSession(epoch);
      return saveGrantedDownload(
        owner,
        entry.id,
        grant,
        entry.name,
        pause: pause,
      );
    });
    _checkSession(epoch);
    try {
      await finishDownload(owner, id, actualName);
    } on TaskStoreFailure {
      if (actualName == null) rethrow;
      throw AccountFailure('已保存「$actualName」，本机任务记录写入失败。');
    }
    return actualName;
  }

  Future<T> _withPauseRecord<T>(
    AccountOwner owner,
    String id,
    Future<T> Function() action,
  ) async {
    try {
      return await action();
    } on AccountFailure catch (error) {
      final reason = switch (error) {
        TransferPaused() => 'manual',
        MultipartSourceUnavailable() => 'source',
        TransferTargetUnavailable() => 'target',
        TransferSpaceUnavailable() => 'space',
        ReauthenticationRequired() => 'authorization',
        _ => 'interrupted',
      };
      await setTransferPauseReason(owner, id, reason);
      rethrow;
    }
  }

  Future<String> cancelTransfer(
    AccountOwner taskOwner,
    String localId, {
    required bool submit,
  }) async {
    if (owner != taskOwner || _signingOut) {
      throw const ReauthenticationRequired();
    }
    final epoch = _sessionEpoch;
    try {
      // 1. 等待用户确认，保存原任务的取消意图。
      final task = await readCancellationTask(taskOwner, localId, submit);
      _checkSession(epoch);
      switch (task) {
        case DownloadCancellation(:final source):
          // 2. 下载只清理其私有断点。失败时保留待确认记录以便重试。
          await discardDownloadCheckpoint(
            taskOwner,
            source.entry,
            source.objectId,
            source.sha256,
          );
          await finishDownloadCancellation(taskOwner, localId);
          _checkSession(epoch);
          return '下载已取消。';
        case final UploadCancellation upload:
          return await _cancelUpload(taskOwner, localId, upload, submit, epoch);
        case null:
          throw const AccountFailure('原任务已结束或不可取消。');
      }
    } on TaskStoreFailure {
      throw const AccountFailure('无法更新原任务的取消记录，请重试。');
    }
  }

  Future<String> _cancelUpload(
    AccountOwner owner,
    String localId,
    UploadCancellation upload,
    bool submit,
    int epoch,
  ) async {
    final source = upload.source;
    final parentId = upload.parentId;
    final multipart = source.sizeBytes > maxSingleUploadBytes;
    if (upload.stage != UploadSubmissionStage.create) {
      if (!submit) return '文件复用结果待确认，可重试取消。';
      // 复用回执可能已经发布。用原操作和原键核对，不进入普通创建。
      final receipt = upload.stage == UploadSubmissionStage.reusePending
          ? await _readForAccount(
              (session) =>
                  reuseUploadContent(session, localId, parentId, source),
            )
          : null;
      _checkSession(epoch);
      if (receipt == null) {
        await cancelUncreatedUpload(owner, localId);
      } else {
        await saveUploadReuse(owner, localId, receipt);
      }
      if (multipart) {
        await releaseMultipartSource(taskDocumentScope(owner, localId));
      }
      _checkSession(epoch);
      return receipt == null ? '上传已取消。' : '上传已发布，可在原目录查看。';
    }
    var remoteId = upload.remoteId;
    if (remoteId == null) {
      if (!submit) return '原上传创建结果待确认，请重试取消。';
      // 创建回执丢失时，用原键找回任务。取消流程无需读取或发送正文。
      if (multipart) {
        final plan = await _readForAccount(
          (session) =>
              createMultipartUpload(session, localId, parentId, source),
        );
        await saveMultipartCheckpoint(owner, localId, plan);
        remoteId = plan.taskId;
      } else {
        remoteId = await _readForAccount<String>(
          (session) => createSingleUpload(session, localId, parentId, source),
        );
        await saveSingleCheckpoint(owner, localId, remoteId, 'PENDING');
      }
    }
    final target = (
      id: remoteId,
      mode: multipart ? 'MULTIPART' : 'SINGLE',
      sizeBytes: source.sizeBytes,
    );
    Future<UploadTaskSnapshot> readTask() {
      _checkSession(epoch);
      return _readForAccount(
        (session) => readUploadTaskStatus(session, target, parentId),
      );
    }

    // 2. 先查询实际状态。回执丢失时只对账，当前操作不重复发送取消。
    var snapshot = await readTask();
    if (submit &&
        upload.state != 'CANCELLING' &&
        snapshot.state != 'CANCELLING' &&
        snapshot.state != 'EXPIRING' &&
        !uploadIsTerminal(snapshot.state)) {
      try {
        snapshot = await _readForAccount(
          (session) => requestUploadCancellation(session, target, parentId),
        );
      } on ReauthenticationRequired {
        rethrow;
      } on AccountFailure {
        snapshot = await readTask();
        if (snapshot.state != 'CANCELLING' &&
            !uploadIsTerminal(snapshot.state)) {
          rethrow;
        }
      }
    }

    if (snapshot.state == 'CANCELLING') {
      await recordUploadCancelling(owner, localId, target.id);
    }
    final waiting = Stopwatch()..start();
    while (snapshot.state == 'CANCELLING' || snapshot.state == 'EXPIRING') {
      if (waiting.elapsed >= const Duration(seconds: 30)) {
        return '服务端仍在处理，可在任务记录查看结果。';
      }
      await Future<void>.delayed(const Duration(milliseconds: 450));
      snapshot = await readTask();
    }
    if (!uploadIsTerminal(snapshot.state)) return '取消尚未确认，可在任务记录重试。';

    // 3. 按实际终态更新记录。发布先完成时保留有效的发布条目。
    _checkSession(epoch);
    await saveUploadOutcome(owner, localId, target.id, snapshot);
    if (multipart) {
      await releaseMultipartSource(taskDocumentScope(owner, localId));
    }
    _checkSession(epoch);
    return switch (snapshot.state) {
      'CANCELLED' => '上传已取消。',
      'COMPLETED' => '上传已发布，可在原目录查看。',
      'EXPIRED' => '原上传任务已过期。',
      _ => '原上传任务已失败。',
    };
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
    TransferPause? pause,
    String? uploadName,
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
      final intent = await reserveMultipartIntent(currentOwner, parentId, (
        name: uploadName ?? source.name,
        sizeBytes: source.sizeBytes,
        sha256: source.sha256,
      ));
      _checkSession(epoch);
      return await _withPauseRecord(
        currentOwner,
        intent.id,
        () => _runMultipart(
          currentOwner,
          intent,
          epoch,
          onProgress,
          pause,
          verifyTarget: intent.creation != null,
        ),
      );
    } on TaskStoreFailure {
      throw const AccountFailure('无法读写本机上传记录，请重试。');
    }
  }

  Future<String?> resumeMultipart(
    AccountOwner taskOwner,
    String localId, {
    required bool reselect,
    required void Function(MultipartCreation plan, int confirmed) onProgress,
    TransferPause? pause,
  }) async {
    if (owner != taskOwner || _signingOut) {
      throw const ReauthenticationRequired();
    }
    final epoch = _sessionEpoch;
    try {
      // 1. 按账号和本地任务 ID 找回原上传记录。
      final pending = await readPendingMultipartUpload(taskOwner, localId);
      _checkSession(epoch);
      if (pending == null) throw const AccountFailure('原任务已经结束或不可继续。');

      return await _withPauseRecord(taskOwner, localId, () async {
        pause?.check();
        // 2. 只有大小和 SHA-256 一致的副本才能替换该任务的来源。
        if (reselect &&
            !await pickReplacementMultipartSource(
              taskDocumentScope(taskOwner, localId),
              pending.source,
            )) {
          return null;
        }
        _checkSession(epoch);
        return _runMultipart(
          taskOwner,
          pending,
          epoch,
          onProgress,
          pause,
          verifyTarget: true,
        );
      });
    } on TaskStoreFailure {
      throw const AccountFailure('无法读写本机上传记录，请重试。');
    }
  }

  Future<String> _runMultipart(
    AccountOwner currentOwner,
    PendingMultipartUpload intent,
    int epoch,
    void Function(MultipartCreation plan, int confirmed) onProgress,
    TransferPause? pause, {
    required bool verifyTarget,
  }) async {
    final source = intent.source;
    final parentId = intent.parentId;
    final scope = taskDocumentScope(currentOwner, intent.id);
    pause?.check();

    Future<void>? binding;
    Future<void> bindSource() => binding ??= bindMultipartSource(
      scope,
      documentScopeFor(currentOwner),
      source,
    );
    final reused = await _tryUploadReuse(
      currentOwner,
      intent.id,
      intent.stage,
      parentId,
      source,
      epoch,
      pause: pause,
      verifySource: () async {
        await bindSource();
        await verifyUploadReuseSource(scope, source);
      },
    );
    if (reused != null) {
      await releaseMultipartSource(scope);
      _checkSession(epoch);
      return reused;
    }
    pause?.check();
    if (intent.creation == null) {
      await bindSource();
      _checkSession(epoch);
      pause?.check();
    }
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
    Future<UploadTaskSnapshot> queryTask() => _readForAccount(
      (session) => readMultipartTask(session, plan, parentId),
    );
    final waiting = Stopwatch()..start();
    var task = await queryTask();
    while (task.state == 'CREATING') {
      pause?.check();
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
        pause: pause,
      );
    }
    if (!task.expiresAt.isAfter(DateTime.now())) {
      throw const AccountFailure('原分片任务的有效期已结束。');
    }

    pause?.check();
    if (verifyTarget) await _verifyUploadTarget(parentId, epoch);
    pause?.check();
    if (intent.creation != null) {
      await bindSource();
      _checkSession(epoch);
    }
    pause?.check();
    final digests = await inspectMultipartSource(scope, source, plan);
    _checkSession(epoch);
    pause?.check();
    final remote = await _readForAccount(
      (session) => reconcileMultipartReceipts(session, plan, digests),
    );

    // 3. 沿用原远端任务，对账后补传缺片。
    await saveReconciledMultipartParts(
      currentOwner,
      intent.id,
      plan.taskId,
      remote.values,
    );
    final saved = await readMultipartReceipts(currentOwner, intent.id, plan);
    _checkSession(epoch);
    pause?.check();
    await setTransferPauseReason(currentOwner, intent.id, null);
    _checkSession(epoch);
    var confirmed = saved.length;
    onProgress(plan, confirmed);
    for (var number = 1; number <= plan.partCount; number++) {
      if (saved.containsKey(number)) continue;
      _checkSession(epoch);
      pause?.check();
      final part = (
        number: number,
        offsetBytes: plan.offsetOf(number),
        sizeBytes: plan.sizeOf(number),
        sha256: digests[number - 1],
      );
      try {
        // 签名明确过期时，核对原任务后重签一次。
        final receipt = await _sendMultipartWithRenewal(
          scope,
          source,
          part,
          plan,
          parentId,
          pause,
        );
        await saveMultipartPart(currentOwner, intent.id, plan.taskId, receipt);
        _checkSession(epoch);
        saved[number] = receipt;
        onProgress(plan, ++confirmed);
      } on TransferPaused {
        rethrow;
      } on ReauthenticationRequired {
        rethrow;
      } on MultipartSourceUnavailable {
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
      pause: pause,
    );
  }

  Future<String?> _tryUploadReuse(
    AccountOwner owner,
    String id,
    UploadSubmissionStage stage,
    String parentId,
    PreparedUploadFile source,
    int epoch, {
    required Future<void> Function() verifySource,
    TransferPause? pause,
  }) async {
    if (stage == UploadSubmissionStage.create) return null;

    // 1. 首次复核文件并保存阶段。回执待确认时直接沿用原声明和键。
    if (stage == UploadSubmissionStage.reuseReady) {
      await verifySource();
      _checkSession(epoch);
      pause?.check();
      await setUploadSubmissionStage(
        owner,
        id,
        UploadSubmissionStage.reusePending,
      );
    }

    // 2. 请求内容复用，命中时记录服务端返回的目录项。
    _checkSession(epoch);
    pause?.check();
    final receipt = await _readForAccount(
      (session) => reuseUploadContent(session, id, parentId, source),
    );
    if (receipt != null) {
      await saveUploadReuse(owner, id, receipt);
      _checkSession(epoch);
      return receipt.entryId;
    }

    // 3. 明确未命中时先记录回退。恢复普通创建时仍走原创建接口。
    await setUploadSubmissionStage(owner, id, UploadSubmissionStage.create);
    _checkSession(epoch);
    return null;
  }

  Future<void> _verifyUploadTarget(String parentId, int epoch) async {
    try {
      final profile = await currentProfile();
      _checkSession(epoch);
      await listDirectory(rootId: profile.rootEntryId, parentId: parentId);
    } on ReauthenticationRequired {
      rethrow;
    } on AccountFailure {
      throw const TransferTargetUnavailable('无法核对原目标目录，请检查后继续。');
    }
  }

  Future<MultipartReceipt> _sendMultipartWithRenewal(
    String scope,
    PreparedUploadFile source,
    MultipartPart part,
    MultipartCreation plan,
    String parentId,
    TransferPause? pause,
  ) async {
    final grant = await _readForAccount(
      (session) => requestMultipartPart(session, plan, part),
    );
    pause?.check();
    try {
      return await sendMultipartPart(scope, source, part, grant);
    } on MultipartSignatureExpired {
      pause?.check();
      final renewed = await _readForAccount((session) async {
        if (!await multipartReady(session, plan, parentId)) {
          throw const AccountFailure('原任务暂未准备好，请重试。');
        }
        return renewMultipartPart(session, plan, part, grant);
      });
      pause?.check();
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
    UploadTaskSnapshot? initialTask,
    TransferPause? pause,
  }) async {
    Future<UploadTaskSnapshot> readTask() {
      _checkSession(epoch);
      return _readForAccount(
        (session) => readMultipartTask(session, plan, parentId),
      );
    }

    // 1. 查询原任务，按片号核对已保存的完整回执。
    var task = initialTask ?? await readTask();
    if (task.state == 'UPLOADING') {
      pause?.check();
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
        await saveUploadOutcome(owner, localId, plan.taskId, task);
        await releaseMultipartSource(taskDocumentScope(owner, localId));
        _checkSession(epoch);
        return entryId;
      }
      if (const {'FAILED', 'EXPIRED', 'CANCELLED'}.contains(task.state)) {
        await saveUploadOutcome(owner, localId, plan.taskId, task);
        await releaseMultipartSource(taskDocumentScope(owner, localId));
        _checkSession(epoch);
        final label = switch (task.state) {
          'EXPIRED' => '原分片任务已过期',
          'CANCELLED' => '原分片任务已取消',
          _ => multipartFailureLabel(task.errorCode),
        };
        throw AccountFailure('$label。文件未发布。');
      }
      pause?.check();
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
    String parentId, {
    String? uploadName,
  }) async {
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
      final declaration = (
        name: uploadName ?? source.name,
        sizeBytes: source.sizeBytes,
        sha256: source.sha256,
      );
      final intent = await reserveSingleIntent(
        currentOwner,
        parentId,
        declaration,
      );
      _checkSession(epoch);
      final reused = await _tryUploadReuse(
        currentOwner,
        intent.id,
        intent.stage,
        parentId,
        declaration,
        epoch,
        verifySource: () =>
            verifyUploadReuseSource(documentScopeFor(currentOwner), source),
      );
      if (reused != null) return reused;
      final String taskId;
      if (intent.remoteId case final String existing) {
        taskId = existing;
      } else {
        await openBody();
        taskId = await remote(
          (session) =>
              createSingleUpload(session, intent.id, parentId, declaration),
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
