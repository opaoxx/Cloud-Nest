import 'package:material_ui/material_ui.dart';

import 'document_selection_page.dart';

void main() {
  // 1. 交给 Flutter 驱动应用的根组件。
  runApp(const CloudNestApp());
}

class CloudNestApp extends StatelessWidget {
  const CloudNestApp({super.key});

  @override
  Widget build(BuildContext context) {
    // 2. 将系统文件选择入口放在首页。
    return const MaterialApp(
      title: 'CloudNest',
      debugShowCheckedModeBanner: false,
      home: DocumentSelectionPage(),
    );
  }
}
