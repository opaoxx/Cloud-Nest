import 'dart:async';
import 'dart:io';

import 'package:flutter/services.dart';

import 'account_owner.dart';
import 'native_directory.dart';
import 'native_login.dart';
import 'native_storage_url.dart';
import 'transfer_pause.dart';

const _downloads = MethodChannel('cloudnest/android_download');
final _digestPattern = RegExp(r'^[0-9a-f]{64}$');

typedef DownloadGrant = ({
  String signedUrl,
  String objectId,
  String revision,
  int sizeBytes,
  String sha256,
});

Future<DownloadGrant> createDownloadGrant(
  NativeSession session,
  DirectoryEntry entry,
) async {
  // 1. 绑定目录页所见的文件修订，服务端可拒绝旧版本的请求。
  if (entry.isFolder ||
      !isCloudNestId(entry.id) ||
      !isCloudNestId(entry.revision)) {
    throw const AccountFailure('文件条目无效，请刷新目录。');
  }
  final result = await requestAuthorizedJson(
    session.address,
    session.accessToken,
    '/api/v1/entries/${entry.id}/download',
    payload: {'expected_revision': entry.revision, 'purpose': 'download'},
  );

  if (result.status == HttpStatus.conflict ||
      result.status == HttpStatus.notFound) {
    throw const TransferTargetUnavailable('文件已变化或当前不可下载，请刷新目录后重新选择。');
  }
  if (result.status != HttpStatus.ok) {
    throw const AccountFailure('服务端未交付下载授权，请重试。');
  }

  // 2. 核对授权的修订、大小和摘要，保留签名 URL 原文。
  if (result.body
      case {
        'data': {
          'url': final String raw,
          'expires_at': final String expiresText,
          'object_id': final String objectId,
          'revision': final String revision,
          'size_bytes': final int size,
          'sha256': final String sha,
          'disposition': final String disposition,
        },
      }
      when isCloudNestId(objectId) &&
          revision == entry.revision &&
          size == entry.sizeBytes &&
          _digestPattern.stringMatch(sha) == sha &&
          disposition.startsWith('attachment;')) {
    final expiresAt = DateTime.tryParse(expiresText);
    if (expiresAt != null &&
        expiresAt.isAfter(DateTime.now()) &&
        parseStorageUrl(raw, session.address) != null) {
      return (
        signedUrl: raw,
        objectId: objectId,
        revision: revision,
        sizeBytes: size,
        sha256: sha,
      );
    }
  }
  throw const AccountFailure('下载授权无效或已过期，请重新下载。');
}

Future<String?> saveGrantedDownload(
  AccountOwner owner,
  String entryId,
  DownloadGrant grant,
  String suggestedName, {
  TransferPause? pause,
}) async {
  // 3. 安卓负责存储 GET、校验和另存为。返回的名称来自用户选择的提供方。
  try {
    pause?.check();
    final pending = _downloads.invokeMapMethod<String, Object?>(
      'downloadAndSave',
      {
        'url': grant.signedUrl,
        'server': owner.server,
        'userId': owner.userId,
        'entryId': entryId,
        'objectId': grant.objectId,
        'revision': grant.revision,
        'name': suggestedName,
        'sizeBytes': grant.sizeBytes,
        'sha256': grant.sha256,
      },
    );
    pause?.notifyWhileActive(() => unawaited(_notifyDownloadPause()));
    final result = await pending;
    if (result == null) return null;
    if (result case {'name': final String name} when name.isNotEmpty) {
      return name;
    }
    throw const AccountFailure('文件已保存，但目标提供方没有返回实际名称。');
  } on PlatformException catch (error) {
    if (error.code == 'download_paused' || pause?.requested == true) {
      throw const TransferPaused();
    }
    if (error.code == 'download_space') throw const TransferSpaceUnavailable();
    throw AccountFailure(switch (error.code) {
      'download_integrity' => '下载校验失败，文件未保存。请重试。',
      'download_interrupted' => '下载中断。再次点击下载可继续。',
      'download_range' => '存储返回的正文范围有误，已保留原断点。请重试。',
      'download_picker_unavailable' => '无法打开系统另存为，请重试。',
      'download_save_failed' => '无法写入所选位置，请重试并另选位置。',
      'download_save_incomplete' => '写入未完成，所选位置可能留有未写完的文件。',
      'download_closed' => '下载窗口已关闭，请重试。',
      'download_busy' => '正在处理另一个下载。',
      _ => '无法读取下载正文，请重试。',
    });
  } on MissingPluginException {
    throw const AccountFailure('当前设备无法选择保存位置。');
  } finally {
    pause?.notifyWhileActive(null);
  }
}

Future<void> _notifyDownloadPause() async {
  try {
    await _downloads.invokeMethod<void>('pauseDownload');
  } on PlatformException {
    // 下载调用本身会报告 Android 是否接受了暂停。
  } on MissingPluginException {
    // 下载调用会报告当前平台不可用。
  }
}
