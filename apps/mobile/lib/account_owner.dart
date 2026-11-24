import 'dart:convert';

typedef AccountOwner = ({String server, String userId});

String documentScopeFor(AccountOwner? owner) =>
    owner == null ? 'guest' : jsonEncode([owner.server, owner.userId]);
