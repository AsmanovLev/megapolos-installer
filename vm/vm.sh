#!/usr/bin/env bash
# =============================================================================
# vm.sh — управление VM: start | stop | status | ssh | reset | destroy | log
#
#   ./vm/vm.sh <имя> <команда>
#
# reset — пересоздать overlay поверх базового образа ("снапшот чистой системы")
#         и запустить VM заново. Основной цикл отладки установщика.
# =============================================================================
set -euo pipefail
WORKSPACE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RUN="$WORKSPACE/vm/run"

NAME="${1:-}"; CMD="${2:-}"
[ -n "$NAME" ] && [ -n "$CMD" ] || { echo "usage: $0 <имя> start|stop|status|ssh|reset|destroy|log" >&2; exit 1; }
CONF="$RUN/$NAME/vm.conf"
[ -f "$CONF" ] || { echo "VM '$NAME' не найдена (создай: ./vm/create-vm.sh $NAME)" >&2; exit 1; }
# shellcheck disable=SC1090
. "$CONF"
CONSOCK="${CONSOCK:-$RUN/$NAME/console.sock}"

is_running() { [ -f "$PIDFILE" ] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; }

# LAN-IP bridge-VM по стабильному MAC (neigh-таблица хоста).
# Запись появляется только после трафика с VM → при промахе делаем ping-sweep подсети бриджа.
resolve_ip() {
  [ -n "${MAC:-}" ] || return 1
  local ip
  ip="$(ip neigh | grep -i "$MAC" | grep -oE '^[0-9.]+' | head -1)"
  if [ -z "$ip" ]; then
    local subnet
    subnet="$(ip -4 addr show "${BRIDGE:-br0}" | grep -oP 'inet \K[0-9.]+/[0-9]+' | head -1)"
    if [ -n "$subnet" ]; then
      local base="${subnet%.*}" i
      for i in $(seq 1 254); do ping -c1 -W1 "$base.$i" >/dev/null 2>&1 & done
      wait
      ip="$(ip neigh | grep -i "$MAC" | grep -oE '^[0-9.]+' | head -1)"
    fi
  fi
  [ -n "$ip" ] && echo "$ip"
}

start_vm() {
  if is_running; then echo "уже запущена (pid $(cat "$PIDFILE"))"; return; fi
  if [ "${NET:-user}" = "bridge" ]; then
    # tap через setuid qemu-bridge-helper → VM в LAN (DHCP роутера)
    qemu-system-x86_64 \
      -machine q35,accel=kvm -cpu host -m "$RAM" -smp "$CPUS" \
      -drive "file=$OVERLAY,if=virtio,format=qcow2" \
      -drive "file=$SEED,if=virtio,format=raw,readonly=on" \
      -netdev "tap,id=n0,br=${BRIDGE:-br0},helper=/usr/libexec/qemu-bridge-helper" \
      -device "virtio-net-pci,netdev=n0,mac=$MAC" \
      -display none \
      -chardev "socket,id=ser0,path=$CONSOCK,server=on,wait=off,logfile=$CONSOLE" \
      -serial chardev:ser0 \
      -daemonize -pidfile "$PIDFILE"
    echo "запущена (bridge ${BRIDGE:-br0}, mac $MAC). LAN-IP: ./vm/vm.sh $NAME status"
    return
  fi
  # GUI static nginx в госте: 8080/4443 (80/443 заняты nginx-контейнером ноды платформы)
  local fwds="hostfwd=tcp::$SSH_PORT-:22,hostfwd=tcp::$GUI_PORT-:8080,hostfwd=tcp::$API_PORT-:5100"
  [ -n "${GUI_TLS_PORT:-}" ] && fwds="$fwds,hostfwd=tcp::$GUI_TLS_PORT-:4443"
  qemu-system-x86_64 \
    -machine q35,accel=kvm -cpu host -m "$RAM" -smp "$CPUS" \
    -drive "file=$OVERLAY,if=virtio,format=qcow2" \
    -drive "file=$SEED,if=virtio,format=raw,readonly=on" \
    -netdev "user,id=n0,$fwds" \
    -device virtio-net-pci,netdev=n0 \
    -display none \
      -chardev "socket,id=ser0,path=$CONSOCK,server=on,wait=off,logfile=$CONSOLE" \
      -serial chardev:ser0 \
    -daemonize -pidfile "$PIDFILE"
  echo "запущена. ssh: ssh -p $SSH_PORT megapolos@localhost  (готовность ~30-60 сек)"
}

stop_vm() {
  if is_running; then
    ssh_vm 'sudo poweroff' >/dev/null 2>&1 || true
    for _ in $(seq 1 20); do is_running || break; sleep 2; done
    is_running && kill "$(cat "$PIDFILE")" 2>/dev/null || true
    rm -f "$PIDFILE"
    echo "остановлена"
  else echo "не запущена"; fi
}

ssh_vm() {
  if [ "${NET:-user}" = "bridge" ]; then
    local ip; ip="$(resolve_ip)"
    [ -n "$ip" ] || { echo "LAN-IP не найден (VM ещё не получила DHCP? mac=$MAC)" >&2; return 1; }
    ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
      megapolos@"$ip" "$@"
    return
  fi
  ssh -p "$SSH_PORT" \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
    megapolos@localhost "$@"
}

case "$CMD" in
  start)   start_vm ;;
  stop)    stop_vm ;;
  status)
    if is_running; then
      if [ "${NET:-user}" = "bridge" ]; then
        echo "running (pid $(cat "$PIDFILE"), bridge ${BRIDGE:-br0}, mac $MAC, ip: $(resolve_ip || echo 'ещё нет DHCP'))"
      else
        echo "running (pid $(cat "$PIDFILE"), ssh :$SSH_PORT, gui :$GUI_PORT, api :$API_PORT)"
      fi
      ssh_vm -o ConnectTimeout=3 'echo ssh: ok' 2>/dev/null || echo "ssh: ещё не готов"
    else echo "stopped"; fi ;;
  ssh)     shift 2; ssh_vm "$@" ;;
  log)     tail -n 80 -f "$CONSOLE" ;;
  console)
    # интерактивная консоль VM (socat → unix socket; выход: Ctrl+O)
    command -v socat >/dev/null || { echo "нужен socat" >&2; exit 1; }
    exec socat -,rawer,escape=0x0f "UNIX-CONNECT:$CONSOCK" ;;
  reset)
    stop_vm >/dev/null 2>&1 || true
    rm -f "$OVERLAY"
    BASE="$WORKSPACE/vm/images/$DISTRO-base.qcow2"
    qemu-img create -f qcow2 -b "$BASE" -F qcow2 "$OVERLAY" "${DISK:-20G}" >/dev/null
    # новый instance-id, чтобы cloud-init отработал заново
    sed -i "s/^instance-id:.*/instance-id: $NAME-$(date +%s)/" "$RUN/$NAME/meta-data"
    genisoimage -quiet -output "$SEED" -volid cidata -joliet -rock \
      "$RUN/$NAME/user-data" "$RUN/$NAME/meta-data"
    # у VM будут новые host-ключи — чистим known_hosts, чтобы ssh не ругался
    ssh-keygen -R "[localhost]:$SSH_PORT" >/dev/null 2>&1 || true
    echo "overlay пересоздан (чистая $DISTRO)"
    start_vm ;;
  destroy)
    stop_vm >/dev/null 2>&1 || true
    rm -rf "$RUN/$NAME" "$OVERLAY"
    ssh-keygen -R "[localhost]:$SSH_PORT" >/dev/null 2>&1 || true
    echo "удалена (базовый образ остался: $WORKSPACE/vm/images/)" ;;
  *) echo "неизвестная команда: $CMD" >&2; exit 1 ;;
esac
