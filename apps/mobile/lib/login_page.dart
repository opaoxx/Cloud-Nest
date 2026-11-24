import 'package:flutter/foundation.dart';
import 'package:material_ui/material_ui.dart';

import 'native_login.dart';

class LoginPage extends StatefulWidget {
  const LoginPage({super.key});

  @override
  State<LoginPage> createState() => _LoginPageState();
}

class _LoginPageState extends State<LoginPage> {
  final _address = TextEditingController();
  final _username = TextEditingController();
  final _password = TextEditingController();
  bool _submitting = false;
  String? _notice;

  Future<void> _submit() async {
    if (_submitting) return;

    // 1. 请求期间锁定表单，避免一次点击产生多个会话。
    setState(() {
      _submitting = true;
      _notice = null;
    });
    try {
      // 2. 成功后把会话交回首页，仅在当前进程内使用。
      final session = await loginAndroid(
        address: _address.text,
        username: _username.text,
        password: _password.text,
      );
      if (!mounted || ModalRoute.of(context)?.isCurrent != true) return;
      _password.clear();
      Navigator.of(context).pop(session);
    } on LoginFailure catch (failure) {
      // 3. 显示固定的失败提示，并恢复重试入口。
      if (!mounted) return;
      setState(() {
        _notice = failure.message;
        _submitting = false;
      });
    }
  }

  @override
  void dispose() {
    _address.dispose();
    _username.dispose();
    _password.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final notice = _notice;
    return Scaffold(
      appBar: AppBar(title: const Text('登录 CloudNest')),
      body: SafeArea(
        child: ListView(
          padding: const EdgeInsets.all(24),
          children: [
            TextField(
              controller: _address,
              enabled: !_submitting,
              keyboardType: TextInputType.url,
              autocorrect: false,
              enableSuggestions: false,
              textInputAction: TextInputAction.next,
              decoration: const InputDecoration(
                labelText: '服务地址',
                hintText: 'https://cloud.example.com',
                helperText: kDebugMode ? '调试包可接入可信局域网的 HTTP 服务' : null,
              ),
            ),
            const SizedBox(height: 12),
            TextField(
              controller: _username,
              enabled: !_submitting,
              autocorrect: false,
              enableSuggestions: false,
              textInputAction: TextInputAction.next,
              decoration: const InputDecoration(labelText: '用户名'),
            ),
            const SizedBox(height: 12),
            TextField(
              controller: _password,
              enabled: !_submitting,
              autocorrect: false,
              enableSuggestions: false,
              obscureText: true,
              textInputAction: TextInputAction.done,
              onSubmitted: (_) => _submit(),
              decoration: const InputDecoration(labelText: '密码'),
            ),
            const SizedBox(height: 20),
            Align(
              alignment: Alignment.centerLeft,
              child: FilledButton(
                onPressed: _submitting ? null : _submit,
                child: Text(_submitting ? '正在登录…' : '登录'),
              ),
            ),
            if (notice != null)
              Semantics(liveRegion: true, child: Text(notice)),
          ],
        ),
      ),
    );
  }
}
