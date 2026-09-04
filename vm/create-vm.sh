#!/usr/bin/env bash
# =============================================================================
# create-vm.sh — создать VM (Ubuntu cloud image + cloud-init) под QEMU/KVM.
#
#   ./vm/create-vm.sh <имя> [опции]
#
# Опции:
#   --distro ubuntu2404|debian12   (по умолчанию ubuntu2404)
#   --ram <MiB>        (по умолчанию 4096)
#   --cpus <N>         (по умолчанию 4)
#   --disk <размер>    (по умолчанию 20G)
#   --ssh-port <порт>  (по умолчанию 2222)  host -> guest :22
#   --gui-port <порт>  (по умолчанию 8080)  host -> guest :80
#   --api-port <порт>  (по умолчанию 5100)  host -> guest :5100
#   --offline          — убрать default route в VM (только канал к хосту,
#                        установка идёт через кэши/бандл — проверка оффлайна)
#
# Базовый образ скачивается ОДИН раз в vm/images/ и не изменяется;
# каждая VM — qcow2-overlay поверх него (пересоздание "чистой" VM = секунды).
# Все файлы VM лежат в рабочей папке (на диске, где есть место), не в /var.
# =============================================================================
set -euo pipefail
WORKSPACE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGES="$WORKSPACE/vm/images"
RUN="$WORKSPACE/vm/run"
mkdir -p "$IMAGES" "$RUN"

declare -A IMG_URL=(
  [ubuntu2404]="https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img"
  [debian12]="https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2"
)

NAME="" DISTRO="ubuntu2404" RAM=4096 CPUS=4 DISK=20G
SSH_PORT=2222 GUI_PORT=8080 API_PORT=5100 GUI_TLS_PORT=8443 OFFLINE=0
NET="user" BRIDGE="br0"

while [ $# -gt 0 ]; do case "$1" in
  --offline) OFFLINE=1; shift;;
  --distro) DISTRO="$2"; shift 2;;
  --ram) RAM="$2"; shift 2;;
  --cpus) CPUS="$2"; shift 2;;
  --disk) DISK="$2"; shift 2;;
  --ssh-port) SSH_PORT="$2"; shift 2;;
  --gui-port) GUI_PORT="$2"; shift 2;;
  --api-port) API_PORT="$2"; shift 2;;
  --gui-tls-port) GUI_TLS_PORT="$2"; shift 2;;
  --net) NET="$2"; shift 2;;
  --bridge) BRIDGE="$2"; shift 2;;
  -h|--help) sed -n '2,22p' "$0"; exit 0;;
  *) [ -z "$NAME" ] && NAME="$1" && shift || { echo "неизвестный аргумент: $1" >&2; exit 1; };;
esac; done
[ -n "$NAME" ] || { echo "укажи имя VM (см. --help)" >&2; exit 1; }
[ -n "${IMG_URL[$DISTRO]:-}" ] || { echo "неизвестный дистрибутив: $DISTRO" >&2; exit 1; }

# --- ssh-ключ для доступа ---
PUBKEY="${SSH_PUBKEY:-}"
if [ -z "$PUBKEY" ]; then
  for k in "$HOME/.ssh/id_ed25519.pub" "$HOME/.ssh/id_rsa.pub"; do
    [ -f "$k" ] && PUBKEY="$(cat "$k")" && break
  done
fi
[ -n "$PUBKEY" ] || { echo "нет ssh-ключа (~/.ssh/id_ed25519.pub). Задай SSH_PUBKEY или создай ключ." >&2; exit 1; }

# --- базовый образ (скачивается один раз) ---
BASE="$IMAGES/$DISTRO-base.qcow2"
if [ ! -f "$BASE" ]; then
  echo "==> скачиваю базовый образ $DISTRO"
  curl -fL -C - -o "$BASE.tmp" "${IMG_URL[$DISTRO]}"
  mv "$BASE.tmp" "$BASE"
fi

# --- overlay поверх базового (мгновенный "снапшот чистой системы") ---
OVERLAY="$IMAGES/$NAME.qcow2"
if [ -f "$OVERLAY" ]; then
  echo "==> overlay $NAME уже существует — оставляю (чистую VM делает: ./vm/vm.sh $NAME reset)"
else
  qemu-img create -f qcow2 -b "$BASE" -F qcow2 "$OVERLAY" "$DISK" >/dev/null
fi

# --- cloud-init ---
SEEDDIR="$RUN/$NAME"
mkdir -p "$SEEDDIR"
cat > "$SEEDDIR/meta-data" <<EOF
instance-id: $NAME-$(date +%s)
local-hostname: $NAME
EOF
cat > "$SEEDDIR/user-data" <<EOF
#cloud-config
hostname: $NAME
users:
  - name: megapolos
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    lock_passwd: true
    ssh_authorized_keys:
      - $PUBKEY
package_update: false
EOF
# --- установщик читает проброшенные порты / адрес хоста ---
# bridge: хост-демоны доступны по LAN-IP хоста; user-net: по 10.0.2.2
HOST_IP=10.0.2.2
if [ "$NET" = "bridge" ]; then
  HOST_IP="$(ip route get 1.1.1.1 2>/dev/null | grep -oP 'src \K[0-9.]+' | head -1)"
  [ -n "$HOST_IP" ] || { echo "не удалось определить LAN-IP хоста" >&2; exit 1; }
  # стабильный MAC из имени VM — по нему vm.sh находит LAN-IP в neigh-таблице
  MAC="52:54:00:$(echo -n "$NAME" | md5sum | cut -c1-6 | sed 's/\(..\)/\1:/g;s/:$//')"
fi
cat >> "$SEEDDIR/user-data" <<EOF
write_files:
  - path: /etc/megapolos-vm.env
    permissions: '0644'
    content: |
      NET=$NET
      HOST_IP=$HOST_IP
      SSH_PORT=$SSH_PORT
      GUI_PORT=$GUI_PORT
      API_PORT=$API_PORT
      GUI_TLS_PORT=$GUI_TLS_PORT
EOF

if [ "$OFFLINE" -eq 1 ]; then
  # оффлайн-режим: без default route VM видит только хост (10.0.2.2)
  cat >> "$SEEDDIR/user-data" <<'EOF'
runcmd:
  - ip route del default || true
EOF
fi
genisoimage -quiet -output "$SEEDDIR/seed.iso" -volid cidata -joliet -rock \
  "$SEEDDIR/user-data" "$SEEDDIR/meta-data"

# --- конфиг VM (используется vm.sh) ---
cat > "$SEEDDIR/vm.conf" <<EOF
NAME=$NAME
DISTRO=$DISTRO
RAM=$RAM
CPUS=$CPUS
SSH_PORT=$SSH_PORT
GUI_PORT=$GUI_PORT
API_PORT=$API_PORT
GUI_TLS_PORT=$GUI_TLS_PORT
NET=$NET
BRIDGE=$BRIDGE
MAC=${MAC:-}
OVERLAY=$OVERLAY
SEED=$SEEDDIR/seed.iso
PIDFILE=$SEEDDIR/qemu.pid
CONSOLE=$SEEDDIR/console.log
EOF

echo
echo "VM '$NAME' создана (сеть: $NET)."
echo "  старт:   ./vm/vm.sh $NAME start"
echo "  ssh:     ./vm/vm.sh $NAME ssh"
if [ "$NET" = "bridge" ]; then
  echo "  LAN-IP появится после загрузки (DHCP роутера): ./vm/vm.sh $NAME status"
else
  echo "  установка Megapolos (в другом терминале хоста: ./vm/host-services.sh start):"
  echo "    ./vm/vm.sh $NAME ssh"
  echo "    curl -fsSL http://10.0.2.2:8000/install/megapolos-install.sh | sudo bash"
  echo "  GUI после установки: http://localhost:$GUI_PORT/"
  echo "  API:                 http://localhost:$API_PORT/"
fi
[ "$OFFLINE" -eq 1 ] && echo "  (VM в оффлайн-режиме: внешняя сеть отключена, установка через кэши хоста)"
echo "  чистая VM заново:    ./vm/vm.sh $NAME reset"
