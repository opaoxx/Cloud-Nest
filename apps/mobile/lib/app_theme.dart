import 'package:flutter/services.dart';
import 'package:material_ui/material_ui.dart';

const _appearance = MethodChannel('cloudnest/android_appearance');

Future<ThemeMode> readAppTheme() async {
  try {
    return switch (await _appearance.invokeMethod<String>('readTheme')) {
      'light' => ThemeMode.light,
      'dark' => ThemeMode.dark,
      _ => ThemeMode.system,
    };
  } on PlatformException {
    return ThemeMode.system;
  } on MissingPluginException {
    return ThemeMode.system;
  }
}

Future<bool> saveAppTheme(ThemeMode mode) async {
  try {
    await _appearance.invokeMethod<void>('writeTheme', mode.name);
    return true;
  } on PlatformException {
    return false;
  } on MissingPluginException {
    return false;
  }
}

Future<ThemeMode?> showAppThemeDialog(BuildContext context, ThemeMode current) {
  const labels = {
    ThemeMode.system: '跟随系统',
    ThemeMode.light: '浅色',
    ThemeMode.dark: '深色',
  };
  return showDialog<ThemeMode>(
    context: context,
    builder: (context) => SimpleDialog(
      title: const Text('界面主题'),
      children: [
        for (final option in labels.entries)
          ListTile(
            selected: option.key == current,
            title: Text(option.value),
            trailing: option.key == current ? const Icon(Icons.check) : null,
            onTap: () => Navigator.of(context).pop(option.key),
          ),
      ],
    ),
  );
}
