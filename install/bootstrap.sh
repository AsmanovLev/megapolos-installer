#!/usr/bin/env bash
# =============================================================================
# bootstrap.sh — входная точка установки megapolos.
#
# Режимы (автовыбор):
#   1) Уже внутри смонтированного бандла (рядом installer + debs/):
#        sudo /mnt/megapolos-bundle/install.sh
#      → просто exec installer, НОЛЬ сети (полный оффлайн).
#   2) Есть локальный sqfs-файл (аргументом или /tmp/megapolos-bundle*.sqfs):
#        sudo bash install.sh /path/to/megapolos-bundle.sqfs
#      → mount + exec installer, НОЛЬ сети.
#   3) Сетевой (зеркало хоста):
#        curl -fsSL http://<host>:8000/install/bootstrap.sh | sudo bash
#      → скачать sqfs → mount → exec installer.
#
# Аргументы после пути к sqfs пробрасываются в installer (напр. --no-tui --yes).
# =============================================================================
set -euo pipefail

[ "$(id -u)" -eq 0 ] || { echo "Запусти от root: sudo bash install.sh" >&2; exit 1; }

SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" 2>/dev/null && pwd || echo /nonexistent)"
MNT=/mnt/megapolos-bundle

run_from() { # $1 = каталог бандла
  echo "==> запускаю installer из $1 (сеть не требуется)"
  exec "$1/installer" --bundle "$1" "$@"
}

# --- режим 1: уже внутри смонтированного бандла -----------------------------
if [ -x "$SELF_DIR/installer" ] && [ -d "$SELF_DIR/debs" ]; then
  run_from "$SELF_DIR" "$@"
fi

# --- режим 2: локальный sqfs-файл --------------------------------------------
SQFS=""
for c in "${1:-}" /tmp/megapolos-bundle.sqfs /tmp/megapolos-bundle-22.04.sqfs; do
  [ -n "$c" ] && [ -f "$c" ] && case "$c" in /*.sqfs) SQFS="$c"; break;; esac
done
# уже смонтирован?
if [ -z "$SQFS" ] && mountpoint -q "$MNT" && [ -x "$MNT/installer" ]; then
  run_from "$MNT" "$@"
fi

# --- режим 3: скачать с зеркала хоста (нужна сеть до хоста) -------------------
if [ -z "$SQFS" ]; then
  HOST_IP="${MEGAPOLOS_HOST_IP:-10.0.2.2}"
  SQFS_URL="${MEGAPOLOS_BUNDLE_URL:-http://$HOST_IP:8000/bundle/megapolos-bundle.sqfs}"
  SQFS=/tmp/megapolos-bundle.sqfs
  echo "==> бандл не найден локально, качаю $SQFS_URL"
  curl -fSL --connect-timeout 3 --retry 3 --retry-delay 3 -C - -o "$SQFS" "$SQFS_URL" || {
    echo "FAIL: не удалось скачать бандл (хост $HOST_IP:8000 недоступен?)." >&2
    echo "      Оффлайн: принеси megapolos-bundle.sqfs файлом и запусти:" >&2
    echo "      sudo bash install.sh /путь/к/megapolos-bundle.sqfs" >&2
    exit 1
  }
fi

mkdir -p "$MNT"
if ! mountpoint -q "$MNT"; then
  if ! mount -o ro,loop "$SQFS" "$MNT"; then
    echo "mount не удался (нет squashfs в ядре?) — распаковываю через unsquashfs" >&2
    command -v unsquashfs >/dev/null || { apt-get update && apt-get install -y squashfs-tools; }
    unsquashfs -d /opt/megapolos-bundle "$SQFS"
    exec /opt/megapolos-bundle/installer --bundle /opt/megapolos-bundle "$@"
  fi
fi

run_from "$MNT" "$@"
