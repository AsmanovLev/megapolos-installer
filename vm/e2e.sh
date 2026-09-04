#!/usr/bin/env bash
# =============================================================================
# e2e.sh — E2E-проверка установщика в VM:
#   reset VM → boot → установка бинарём → ассерты.
#
#   ./vm/e2e.sh <имя-vm>             — онлайн-режим (кэши хоста)
#   ./vm/e2e.sh <имя-vm> --offline   — оффлайн: sqfs-бандл в VM, mount, install
#                                      (на хосте перед этим остановить
#                                      apt-cacher/verdaccio!)
#
# Ассерты: 4 сервиса active, GUI 200, API+JWT → Query, нода создана и
# отдаёт системную информацию.
# =============================================================================
set -euo pipefail
WORKSPACE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
NAME="${1:?usage: $0 <vm> [--offline]}"
OFFLINE="${2:-}"

SSH_PORT=$(. "$WORKSPACE/vm/run/$NAME/vm.conf" && echo "$SSH_PORT")
GUI_PORT=$(. "$WORKSPACE/vm/run/$NAME/vm.conf" && echo "$GUI_PORT")
API_PORT=$(. "$WORKSPACE/vm/run/$NAME/vm.conf" && echo "$API_PORT")
SSH="ssh -p $SSH_PORT -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR megapolos@localhost"

echo "== reset $NAME"
"$WORKSPACE/vm/vm.sh" "$NAME" stop >/dev/null 2>&1 || true
"$WORKSPACE/vm/vm.sh" "$NAME" reset

echo "== жду SSH"
for _ in $(seq 1 60); do $SSH true 2>/dev/null && break; sleep 5; done
$SSH true || { echo "FAIL: ssh не поднялся"; exit 1; }

if [ "$OFFLINE" = "--offline" ]; then
  echo "== заливаю sqfs-бандл и монтирую"
  scp -P "$SSH_PORT" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
    "$WORKSPACE/bundle/megapolos-bundle.sqfs" megapolos@localhost:/tmp/megapolos-bundle.sqfs
  $SSH 'sudo mkdir -p /mnt/megapolos-bundle && sudo mount -o ro,loop /tmp/megapolos-bundle.sqfs /mnt/megapolos-bundle && ls /mnt/megapolos-bundle/installer'
  INSTALLER_CMD='/mnt/megapolos-bundle/installer'
else
  echo "== заливаю installer"
  scp -P "$SSH_PORT" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
    "$WORKSPACE/megapolos-installer" megapolos@localhost:/tmp/installer
  INSTALLER_CMD='/tmp/installer'
fi

echo "== установка (headless, лог в /var/log/megapolos-install.log в VM)"
# /tmp нельзя — его чистит systemd-tmpfiles прямо под открытым логом
$SSH "sudo bash -c 'setsid nohup $INSTALLER_CMD --no-tui --yes > /var/log/megapolos-install.log 2>&1 < /dev/null &'" || true

for _ in $(seq 1 240); do
  if $SSH 'sudo test -f /var/log/megapolos-install.log && sudo grep -q "==> ГОТОВО" /var/log/megapolos-install.log' 2>/dev/null; then break; fi
  if $SSH 'sudo test -f /var/log/megapolos-install.log && sudo grep -q "^FAIL" /var/log/megapolos-install.log' 2>/dev/null; then
    echo "== FAIL, хвост лога:"; $SSH 'sudo tail -40 /var/log/megapolos-install.log'; exit 1
  fi
  sleep 15
done
$SSH 'sudo grep -q "==> ГОТОВО" /var/log/megapolos-install.log' || { echo "FAIL: таймаут установки"; $SSH 'sudo tail -40 /var/log/megapolos-install.log'; exit 1; }
echo "== установка завершена, ассерты:"

# 1. сервисы активны
$SSH 'for s in megapolos-core nginx postgresql docker; do systemctl is-active -q $s || { echo "FAIL: $s не active"; exit 1; }; done; echo "  OK: сервисы active"'

# 2. GUI отдаёт 200
[ "$(curl -s -o /dev/null -w '%{http_code}' -m 10 "http://localhost:$GUI_PORT/")" = 200 ] \
  && echo "  OK: GUI :$GUI_PORT → 200" || { echo "  FAIL: GUI не 200"; exit 1; }

# 3. API + JWT
TOKEN=$($SSH 'sudo cat /root/megapolos-token.txt')
RESP=$(curl -s -m 10 -H "token: $TOKEN" -H 'Content-Type: application/json' -d '{"query":"{ __typename }"}' "http://localhost:$API_PORT/")
echo "$RESP" | grep -q '"__typename":"Query"' \
  && echo "  OK: API+JWT → Query" || { echo "  FAIL: API: $RESP"; exit 1; }

# 4. нода создана и собирает системную инфу
RESP=$(curl -s -m 20 -H "token: $TOKEN" -H 'Content-Type: application/json' -d '{"query":"{ nodesSystemInfo { nodeId cpuCores } }"}' "http://localhost:$API_PORT/")
echo "$RESP" | grep -q '"cpuCores"' \
  && echo "  OK: нода с системной инфой: $RESP" || { echo "  FAIL: nodesSystemInfo: $RESP"; exit 1; }

echo "== E2E ЗЕЛЁНЫЙ: $NAME"
