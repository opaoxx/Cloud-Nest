import 'package:flutter/services.dart';
import 'package:material_ui/material_ui.dart';

const _documentPicker = MethodChannel('cloudnest/android_document');

class DocumentSelectionPage extends StatefulWidget {
  const DocumentSelectionPage({super.key});

  @override
  State<DocumentSelectionPage> createState() => _DocumentSelectionPageState();
}

class _DocumentSelectionPageState extends State<DocumentSelectionPage> {
  bool _selecting = false;
  _DocumentSummary? _document;
  String? _notice;

  Future<void> _selectDocument() async {
    if (_selecting) return;

    // 1. 清除前一次结果，从 Flutter 发起单次选择。
    setState(() {
      _selecting = true;
      _document = null;
      _notice = null;
    });

    _DocumentSummary? document;
    String? notice;
    try {
      final result = await _documentPicker.invokeMapMethod<String, Object?>(
        'pickAndSummarize',
      );

      // 2. null 表示系统选择器取消。正文读取和摘要计算由 Android 完成。
      if (result != null) {
        document = _DocumentSummary.fromResult(result);
      }
    } on PlatformException catch (error) {
      notice = error.code == 'picker_unavailable'
          ? '无法打开系统文件选择器。'
          : '无法读取所选文件，请重新选择。';
    } on MissingPluginException {
      notice = '当前设备无法打开系统文件选择器。';
    } on FormatException {
      notice = '文件读取结果无效，请重新选择。';
    }

    // 3. 结果、失败或取消都结束本次选择，让用户可以再次打开。
    if (!mounted) return;
    setState(() {
      _selecting = false;
      _document = document;
      _notice = notice;
    });
  }

  @override
  Widget build(BuildContext context) {
    final document = _document;
    final notice = _notice;
    return Scaffold(
      appBar: AppBar(title: const Text('CloudNest')),
      body: SafeArea(
        child: ListView(
          padding: const EdgeInsets.all(24),
          children: [
            Align(
              alignment: Alignment.centerLeft,
              child: FilledButton(
                onPressed: _selecting ? null : _selectDocument,
                child: const Text('选择文件'),
              ),
            ),
            if (_selecting) const Text('正在选择或读取文件…'),
            if (notice != null) Text(notice),
            if (document != null) ...[
              const SizedBox(height: 20),
              Text(document.name),
              Text('大小 ${document.sizeBytes} B'),
              Text('SHA-256 ${document.sha256}'),
            ],
          ],
        ),
      ),
    );
  }
}

class _DocumentSummary {
  const _DocumentSummary(this.name, this.sizeBytes, this.sha256);

  factory _DocumentSummary.fromResult(Map<String, Object?> result) {
    if (result
        case {
          'name': final String name,
          'sizeBytes': final int sizeBytes,
          'sha256': final String sha256,
        }
        when sizeBytes >= 0) {
      return _DocumentSummary(name, sizeBytes, sha256);
    }
    throw const FormatException('Invalid Android document summary');
  }

  final String name;
  final int sizeBytes;
  final String sha256;
}
