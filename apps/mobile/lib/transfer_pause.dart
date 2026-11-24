import 'native_login.dart';

class TransferPause {
  bool _requested = false;
  void Function()? _notify;

  bool get requested => _requested;

  void request() {
    if (_requested) return;
    _requested = true;
    _notify?.call();
  }

  void notifyWhileActive(void Function()? notify) {
    _notify = notify;
    if (_requested) notify?.call();
  }

  void check() {
    if (_requested) throw const TransferPaused();
  }
}

class TransferPaused extends AccountFailure {
  const TransferPaused() : super('已暂停。可在任务记录中继续。');
}

class TransferTargetUnavailable extends AccountFailure {
  const TransferTargetUnavailable(super.message);
}

class TransferSpaceUnavailable extends AccountFailure {
  const TransferSpaceUnavailable() : super('手机可用空间不足，释放空间后请继续。');
}
