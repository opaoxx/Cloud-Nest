import 'native_login.dart';
import 'native_upload_creation.dart';

class MultipartCreation {
  const MultipartCreation._(
    this.taskId,
    this.sizeBytes,
    this.partSizeBytes,
    this.partCount,
    this.expiresAt,
  );

  static MultipartCreation? checked({
    required String taskId,
    required int sizeBytes,
    required int partSizeBytes,
    required int partCount,
    required String expiresAt,
  }) {
    // 按向上取整核对片数，末片可以短于计划片长。
    if (!isCloudNestId(taskId) ||
        sizeBytes <= maxSingleUploadBytes ||
        sizeBytes > maxUploadProtocolBytes ||
        partSizeBytes < maxSingleUploadBytes ||
        partSizeBytes > maxUploadProtocolBytes ||
        partSizeBytes % uploadMebibyte != 0 ||
        partCount < 1 ||
        partCount > 10000 ||
        (sizeBytes - 1) ~/ partSizeBytes + 1 != partCount ||
        DateTime.tryParse(expiresAt)?.isUtc != true) {
      return null;
    }
    return MultipartCreation._(
      taskId,
      sizeBytes,
      partSizeBytes,
      partCount,
      expiresAt,
    );
  }

  final String taskId;
  final int sizeBytes;
  final int partSizeBytes;
  final int partCount;
  final String expiresAt;

  String get description => '任务 $taskId · $partCount 片 · 片长 $partSizeBytes B';
}

Future<MultipartCreation> createMultipartUpload(
  NativeSession session,
  String key,
  String parentId,
  PreparedUploadFile source,
) async {
  // 1. 提交已落库的声明。幂等重放同样返回创建时的任务快照。
  final body = await requestUploadCreation(session, key, parentId, source);

  // 2. 核对目标、大小、模式和有效期，再验证分片计划。
  if (body
      case {
        'data': {
          'upload_mode': 'MULTIPART',
          'part_size_bytes': final int partSizeBytes,
          'part_count': final int partCount,
          'expires_at': final String expiresAt,
          'task': {
            'id': final String taskId,
            'parent_id': final String actualParent,
            'size_bytes': final int sizeBytes,
            'upload_mode': 'MULTIPART',
            'status': 'CREATING',
            'expires_at': final String taskExpiresAt,
          },
        },
      }
      when actualParent == parentId &&
          sizeBytes == source.sizeBytes &&
          expiresAt == taskExpiresAt) {
    final creation = MultipartCreation.checked(
      taskId: taskId,
      sizeBytes: sizeBytes,
      partSizeBytes: partSizeBytes,
      partCount: partCount,
      expiresAt: expiresAt,
    );
    if (creation != null) return creation;
  }
  throw const AccountFailure('分片创建回执与本机声明不一致，可以重试同一文件和目录。');
}
