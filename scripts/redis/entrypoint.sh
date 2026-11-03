#!/bin/sh
set -eu

: "${REDIS_USERNAME:?REDIS_USERNAME is required}"
: "${REDIS_PASSWORD:?REDIS_PASSWORD is required}"

case "$REDIS_USERNAME:$REDIS_PASSWORD" in
  *[!a-zA-Z0-9:_-]*)
    echo "Development Redis credentials must use letters, digits, _, -, or :." >&2
    exit 1
    ;;
esac

umask 077
printf 'user default off\nuser %s on >%s ~* &* +@all\n' \
  "$REDIS_USERNAME" "$REDIS_PASSWORD" > /data/users.acl
chown redis:redis /data/users.acl

exec /usr/local/bin/docker-entrypoint.sh redis-server \
  --bind 0.0.0.0 \
  --protected-mode yes \
  --appendonly yes \
  --appendfsync everysec \
  --maxmemory 96mb \
  --maxmemory-policy noeviction \
  --aclfile /data/users.acl
