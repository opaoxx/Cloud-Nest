import 'package:flutter/foundation.dart';

Uri? parseStorageUrl(String raw, Uri service) {
  if (raw.codeUnits.any(
    (char) => char <= 32 || char >= 127 || char == 35 || char == 92,
  )) {
    return null;
  }
  final url = Uri.tryParse(raw);
  if (url == null || url.host.isEmpty || url.authority.contains('@')) {
    return null;
  }
  if (url.scheme == 'https' ||
      (kDebugMode && service.scheme == 'http' && url.scheme == 'http')) {
    return url;
  }
  return null;
}
