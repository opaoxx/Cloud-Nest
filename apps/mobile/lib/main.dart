import 'package:material_ui/material_ui.dart';

import 'home_page.dart';
import 'mobile_account.dart';

void main() {
  // 1. 交给 Flutter 驱动应用的根组件。
  runApp(const CloudNestApp());
}

class CloudNestApp extends StatelessWidget {
  const CloudNestApp({super.key, this.account});

  final MobileAccount? account;

  @override
  Widget build(BuildContext context) {
    // 2. 按会话状态显示登录入口或云盘。
    return MaterialApp(
      title: 'CloudNest',
      debugShowCheckedModeBanner: false,
      home: HomePage(account: account),
    );
  }
}
