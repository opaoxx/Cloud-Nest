import 'dart:convert';

typedef AccountOwner = ({String server, String userId});

String documentScopeFor(AccountOwner? owner) =>
    owner == null ? 'guest' : jsonEncode([owner.server, owner.userId]);

String taskDocumentScope(AccountOwner owner, String taskId) =>
    jsonEncode([owner.server, owner.userId, taskId]);
