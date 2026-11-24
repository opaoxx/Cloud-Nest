import 'package:flutter/foundation.dart';
import 'package:flutter/services.dart';
import 'package:material_ui/material_ui.dart';

import 'native_login.dart';

class LoginPage extends StatefulWidget {
  const LoginPage({super.key});

  @override
  State<LoginPage> createState() => _LoginPageState();
}

class _LoginPageState extends State<LoginPage> {
  static const _scheme = kDebugMode ? 'http' : 'https';
  static final _ipv4Digits = RegExp(r'^[0-9]{0,3}$');
  final _segments = List.generate(4, (_) => TextEditingController());
  final _segmentFocus = List.generate(4, (_) => FocusNode());
  final _port = TextEditingController(text: kDebugMode ? '18081' : '443');
  final _portFocus = FocusNode();
  final _username = TextEditingController();
  final _password = TextEditingController();
  bool _submitting = false;
  String? _notice;

  Future<void> _submit() async {
    if (_submitting) return;

    // 1. 校验 IPv4 和端口，拼接规范地址后锁定登录表单。
    final octets = _segments.map((field) => int.tryParse(field.text)).toList();
    final port = int.tryParse(_port.text);
    if (octets.any((octet) => octet == null || octet < 0 || octet > 255) ||
        port == null ||
        port < 1 ||
        port > 65535) {
      setState(() => _notice = '请输入有效的四段 IPv4 地址和 1～65535 之间的端口。');
      return;
    }
    final address = Uri(scheme: _scheme, host: octets.join('.'), port: port);
    setState(() {
      _submitting = true;
      _notice = null;
    });
    try {
      // 2. 成功后把会话交回首页，由首页安全保存刷新凭证。
      final session = await loginAndroid(
        address: address.toString(),
        username: _username.text,
        password: _password.text,
      );
      if (!mounted || ModalRoute.of(context)?.isCurrent != true) return;
      _password.clear();
      Navigator.of(context).pop(session);
    } on AccountFailure catch (failure) {
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
    for (final segment in _segments) {
      segment.dispose();
    }
    for (final focus in _segmentFocus) {
      focus.dispose();
    }
    _port.dispose();
    _portFocus.dispose();
    _username.dispose();
    _password.dispose();
    super.dispose();
  }

  void _focusNextOctet(int index) {
    final next = index + 1;
    (next < _segmentFocus.length ? _segmentFocus[next] : _portFocus)
        .requestFocus();
  }

  Widget _serverAddress() => InputDecorator(
    decoration: const InputDecoration(
      labelText: '服务地址',
      floatingLabelBehavior: FloatingLabelBehavior.always,
      border: OutlineInputBorder(),
      contentPadding: EdgeInsets.symmetric(horizontal: 8),
    ),
    child: Row(
      children: [
        const Text('$_scheme://'),
        for (var index = 0; index < _segments.length; index++) ...[
          if (index > 0) const Text('.'),
          Expanded(
            flex: 3,
            child: Semantics(
              label: 'IPv4 第 ${index + 1} 段',
              child: TextField(
                key: ValueKey('server-ip-$index'),
                controller: _segments[index],
                focusNode: _segmentFocus[index],
                enabled: !_submitting,
                textAlign: TextAlign.center,
                keyboardType: const TextInputType.numberWithOptions(
                  decimal: true,
                ),
                textInputAction: TextInputAction.next,
                onEditingComplete: () => _focusNextOctet(index),
                inputFormatters: [
                  TextInputFormatter.withFunction((previous, edited) {
                    // 输入分隔符时保留当前数字，把光标移到下一段。
                    if (edited.text.endsWith('.')) {
                      final digits = edited.text.substring(
                        0,
                        edited.text.length - 1,
                      );
                      if (digits.isNotEmpty && _ipv4Digits.hasMatch(digits)) {
                        _focusNextOctet(index);
                        return TextEditingValue(
                          text: digits,
                          selection: TextSelection.collapsed(
                            offset: digits.length,
                          ),
                        );
                      }
                    }
                    return _ipv4Digits.hasMatch(edited.text)
                        ? edited
                        : previous;
                  }),
                ],
                decoration: const InputDecoration(
                  hintText: '—',
                  border: InputBorder.none,
                  contentPadding: EdgeInsets.zero,
                  isDense: true,
                ),
              ),
            ),
          ),
        ],
        const Text(':'),
        Expanded(
          flex: 5,
          child: Semantics(
            label: '服务端口',
            child: TextField(
              key: const ValueKey('server-port'),
              controller: _port,
              focusNode: _portFocus,
              enabled: !_submitting,
              textAlign: TextAlign.center,
              keyboardType: TextInputType.number,
              textInputAction: TextInputAction.next,
              inputFormatters: [
                FilteringTextInputFormatter.digitsOnly,
                LengthLimitingTextInputFormatter(5),
              ],
              decoration: const InputDecoration(
                border: InputBorder.none,
                contentPadding: EdgeInsets.zero,
                isDense: true,
              ),
            ),
          ),
        ),
      ],
    ),
  );

  @override
  Widget build(BuildContext context) {
    final notice = _notice;
    return Scaffold(
      appBar: AppBar(title: const Text('登录 CloudNest')),
      body: SafeArea(
        child: ListView(
          padding: const EdgeInsets.all(24),
          children: [
            _serverAddress(),
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
