import 'package:flutter/services.dart';

import 'account_owner.dart';
import 'native_login.dart';
import 'native_upload_creation.dart';

const _documentPicker = MethodChannel('cloudnest/android_document');

Future<PreparedUploadFile?> pickUploadFile(AccountOwner owner) async {
  try {
    // 1. 选择与摘要仍由 Android 处理，文件授权按当前账号保存。
    final result = await _documentPicker.invokeMapMethod<String, Object?>(
      'pickAndSummarize',
      documentScopeFor(owner),
    );
    if (result == null) return null;
    // 2. 摘要只交给上传逻辑，页面只使用文件名与大小。
    if (result
        case {
          'name': final String name,
          'sizeBytes': final int bytes,
          'sha256': final String digest,
          'sourceMode': 'direct' || 'staged',
        }
        when name.isNotEmpty && bytes >= 0) {
      return (name: name, sizeBytes: bytes, sha256: digest);
    }
    throw const AccountFailure('文件读取结果无效，请重新选择。');
  } on PlatformException catch (error) {
    throw AccountFailure(switch (error.code) {
      'picker_unavailable' => '无法打开系统文件选择器。',
      'document_access_lost' => '文件访问授权已失效，请重新选择。',
      'document_source_changed' => '文件内容已变化，请重新选择。',
      'document_staging_budget' => '暂存达到容量上限，请选择更小或可直接读取的文件。',
      'document_staging_space' => '手机可用空间不足，释放空间后请重新选择文件。',
      _ => '无法读取所选文件，请重新选择。',
    });
  } on MissingPluginException {
    throw const AccountFailure('当前设备无法打开系统文件选择器。');
  }
}
