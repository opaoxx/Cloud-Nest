String fileSizeLabel(int bytes) {
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  var size = bytes.toDouble();
  var unit = 0;
  while (size >= 1024 && unit < units.length - 1) {
    size /= 1024;
    unit++;
  }
  final amount = unit == 0
      ? bytes.toString()
      : size.toStringAsFixed(2).replaceFirst(RegExp(r'\.?0+$'), '');
  return '$amount ${units[unit]}';
}

String uploadProgressLabel(int confirmed, int total) {
  return confirmed == total ? '正在校验文件…' : '上传中 · ${confirmed * 100 ~/ total}%';
}
