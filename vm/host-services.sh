#!/usr/bin/env bash
# =============================================================================
# host-services.sh — сервисы хоста для установки/оффлайна Megapolos.
#
#   ./vm/host-services.sh start|stop|status
#
# Поднимает (всё слушает 0.0.0.0, VM достаёт через 10.0.2.2):
#   :8000  python http.server — установщик + git dumb-http зеркало репозиториев
#   :3142  apt-cacher-ng      — кэш apt-пакетов (podman-контейнер)
#   :4873  verdaccio          — кэширующее npm-зеркало (npx verdaccio)
#
# Кэши живут в vm/cache/ — после первого онлайн-прогона VM может
# устанавливаться полностью оффлайн (./vm/create-vm.sh <имя> --offline).
# Хостовая сеть не изменяется (никаких iptables/dnsmasq/системных настроек).
# =============================================================================
set -euo pipefail
WORKSPACE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CACHE="$WORKSPACE/vm/cache"
RUN="$WORKSPACE/vm/run"
PORT_HTTP=8000 PORT_APT=3142 PORT_NPM=4873
mkdir -p "$CACHE" "$RUN"

CMD="${1:-start}"

ctr() { if command -v podman >/dev/null; then podman "$@"; else docker "$@"; fi; }

start_http() {
  # git-репозитории живут в соседнем workspace (../megapolos); собираем
  # композитный корень из симлинков, чтобы URL не менялись:
  #   /megapolos-core/.git  /megapolos-gui/.git  /install/  /bundle/
  local SRC_WS="$(cd "$WORKSPACE/.." && pwd)/megapolos"
  local SERVE="$RUN/serve"
  mkdir -p "$SERVE"
  for repo in megapolos-core megapolos-gui; do
    [ -d "$SRC_WS/$repo/.git" ] || { echo "WARN: $SRC_WS/$repo не найден — git-зеркало не будет работать"; continue; }
    git -C "$SRC_WS/$repo" update-server-info
    ln -sfn "$SRC_WS/$repo" "$SERVE/$repo"
  done
  ln -sfn "$WORKSPACE/install" "$SERVE/install"
  ln -sfn "$WORKSPACE/bundle" "$SERVE/bundle"
  if [ -f "$RUN/http.pid" ] && kill -0 "$(cat "$RUN/http.pid")" 2>/dev/null; then
    echo "http :$PORT_HTTP уже работает"; return
  fi
  (cd "$SERVE" && setsid nohup python3 -m http.server "$PORT_HTTP" --bind 0.0.0.0 \
    < /dev/null > "$RUN/http.log" 2>&1 & echo $! > "$RUN/http.pid")
  echo "http :$PORT_HTTP — установщик + git-зеркало"
}

start_apt_cacher() {
  mkdir -p "$CACHE/apt-cacher"
  if ! ctr ps --format '{{.Names}}' | grep -qx megapolos-apt-cacher; then
    if ! ctr image exists megapolos/apt-cacher-ng:local; then
      echo "==> собираю образ apt-cacher-ng (один раз, нужен интернет)"
      ctr build -t megapolos/apt-cacher-ng:local -f "$CACHE/Containerfile.apt-cacher-ng" "$CACHE"
    fi
    ctr rm -f megapolos-apt-cacher >/dev/null 2>&1 || true
    ctr run -d --name megapolos-apt-cacher \
      -p "0.0.0.0:$PORT_APT:3142" \
      -v "$CACHE/apt-cacher:/var/cache/apt-cacher-ng:Z" \
      megapolos/apt-cacher-ng:local >/dev/null
  fi
  echo "apt-cacher :$PORT_APT — кэш apt-пакетов"
}

start_verdaccio() {
  if [ -f "$RUN/verdaccio.pid" ] && kill -0 "$(cat "$RUN/verdaccio.pid")" 2>/dev/null; then
    echo "verdaccio :$PORT_NPM уже работает"; return
  fi
  mkdir -p "$CACHE/npm"
  setsid nohup npx -y verdaccio --config "$CACHE/verdaccio.yml" --listen "0.0.0.0:$PORT_NPM" \
    < /dev/null > "$RUN/verdaccio.log" 2>&1 & echo $! > "$RUN/verdaccio.pid"
  echo "verdaccio :$PORT_NPM — npm-зеркало"
}

case "$CMD" in
  start)
    start_http; start_apt_cacher; start_verdaccio
    IP="$(hostname -I 2>/dev/null | awk '{print $1}')"
    echo
    echo "Внутри VM:  curl -fsSL http://10.0.2.2:$PORT_HTTP/install/megapolos-install.sh | sudo bash"
    echo "Из LAN:     curl -fsSL http://$IP:$PORT_HTTP/install/megapolos-install.sh | sudo bash"
    ;;
  stop)
    ctr rm -f megapolos-apt-cacher >/dev/null 2>&1 || true
    for s in http verdaccio; do
      [ -f "$RUN/$s.pid" ] && kill "$(cat "$RUN/$s.pid")" 2>/dev/null; rm -f "$RUN/$s.pid"
    done
    echo "остановлено" ;;
  status)
    for p in $PORT_HTTP $PORT_APT $PORT_NPM; do
      curl -s --max-time 2 -o /dev/null "http://127.0.0.1:$p" && echo ":$p OK" || echo ":$p DOWN"
    done ;;
  *) echo "usage: $0 start|stop|status" >&2; exit 1 ;;
esac
