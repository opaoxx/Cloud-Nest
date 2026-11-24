import 'package:material_ui/material_ui.dart';

void main() {
  // 1. 交给 Flutter 驱动应用的根组件。
  runApp(const CloudNestApp());
}

class CloudNestApp extends StatelessWidget {
  const CloudNestApp({super.key});

  @override
  Widget build(BuildContext context) {
    // 2. Android 完成启动后，在安全区域显示可核对的应用名称。
    return const MaterialApp(
      title: 'CloudNest',
      debugShowCheckedModeBanner: false,
      home: Scaffold(
        body: SafeArea(child: Center(child: Text('CloudNest'))),
      ),
    );
  }
}
