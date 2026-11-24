import 'package:material_ui/material_ui.dart';

Future<String?> showFileNameDialog(
  BuildContext context, {
  required String title,
  required String initialName,
  required String actionLabel,
  bool autofocus = true,
  List<Widget> details = const [],
}) {
  final form = GlobalKey<FormState>();
  var name = initialName;
  return showDialog<String>(
    context: context,
    builder: (context) {
      void submit() {
        if (form.currentState?.validate() == true) {
          Navigator.of(context).pop(name.trim());
        }
      }

      return AlertDialog(
        title: Text(title),
        scrollable: true,
        content: Form(
          key: form,
          child: Column(
            mainAxisSize: MainAxisSize.min,
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              ...details,
              TextFormField(
                initialValue: initialName,
                autofocus: autofocus,
                textInputAction: TextInputAction.done,
                decoration: const InputDecoration(labelText: '文件名'),
                onChanged: (value) => name = value,
                onFieldSubmitted: (_) => submit(),
                validator: (value) =>
                    value == null || value.trim().isEmpty ? '请输入文件名。' : null,
              ),
            ],
          ),
        ),
        actions: [
          TextButton(
            onPressed: () => Navigator.of(context).pop(),
            child: const Text('取消'),
          ),
          FilledButton(onPressed: submit, child: Text(actionLabel)),
        ],
      );
    },
  );
}
