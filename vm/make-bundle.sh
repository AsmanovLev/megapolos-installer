#!/usr/bin/env bash
# make-bundle.sh — собирает оффлайн-бандл установки megapolos из УЖЕ установленной VM.
#
# Бандл = bundle/
#   installer             — Go-установщик (запускать внутри целевой VM: sudo ./installer)
#   install.sh            — bash-установщик (legacy/запасной)
#   debs/*.deb            — все скачанные пакеты (ставятся через dpkg -i)
#   npm-cache.tar.gz      — npm-кэш сервисного юзера (npm install --offline)
#   npm-cache-root.tar.gz — npm-кэш root (глобальные nodemon/ts-node)
#   repos/*.git           — git-репозитории
#   pg/newpostgresql.sql  — дамп БД
#   package-lock.*.json   — lock-файлы (в git их нет, а npm --offline требует lock)
# Наружу отдаётся megapolos-bundle.sqfs (squashfs, монтируется без распаковки).
#
# Использование: ./vm/make-bundle.sh [ssh-порт VM]
set -euo pipefail
cd "$(dirname "$0")/.."
WORKSPACE="$PWD"
BUNDLE="$WORKSPACE/bundle"
PORT="${1:-2222}"
SSH="ssh -p $PORT -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR megapolos@localhost"
SCP="scp -P $PORT -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"
# контейнерный движок: podman в приоритете, docker как fallback
ENGINE="$(command -v podman || command -v docker || true)"
[ -n "$ENGINE" ] || { echo "нужен podman или docker" >&2; exit 1; }

echo "== проверяю VM (ssh :$PORT) и наличие установленного megapolos"
$SSH 'test -d /opt/megapolos/megapolos-core/node_modules && test -d /opt/megapolos/megapolos-gui/node_modules && test -d /home/megapolos/.npm && echo VM_OK' | grep -q VM_OK \
  || { echo "В VM нет установленного megapolos (node_modules/.npm). Сначала установи." >&2; exit 1; }

echo "== чищу и создаю bundle/"
rm -rf "$BUNDLE"
mkdir -p "$BUNDLE/debs" "$BUNDLE/repos" "$BUNDLE/pg"

echo "== npm-кэш из VM"
$SSH 'sudo -u megapolos tar czf /tmp/npm-cache.tar.gz -C /home/megapolos .npm'
$SCP megapolos@localhost:/tmp/npm-cache.tar.gz "$BUNDLE/npm-cache.tar.gz"
$SSH 'rm -f /tmp/npm-cache.tar.gz'
# root-кэш (глобальные nodemon/ts-node ставятся от root)
$SSH 'sudo tar czf /tmp/npm-cache-root.tar.gz -C /root .npm'
$SCP megapolos@localhost:/tmp/npm-cache-root.tar.gz "$BUNDLE/npm-cache-root.tar.gz"
$SSH 'sudo rm -f /tmp/npm-cache-root.tar.gz'
# node-gyp headers (sqlite3 компилируется из исходников; без кэша node-gyp
# качает headers с nodejs.org — при мёртвой сети виснет бесконечно)
if $SSH 'test -d /home/megapolos/.cache/node-gyp'; then
  echo "== node-gyp headers cache из VM"
  $SSH 'sudo -u megapolos tar czf /tmp/node-gyp-cache.tar.gz -C /home/megapolos .cache/node-gyp'
  $SCP megapolos@localhost:/tmp/node-gyp-cache.tar.gz "$BUNDLE/node-gyp-cache.tar.gz"
  $SSH 'rm -f /tmp/node-gyp-cache.tar.gz'
else
  echo "WARN: в VM нет ~/.cache/node-gyp — sqlite3 в оффлайне не соберётся" >&2
fi

# ansible-коллекции (community.docker/general/crypto) — galaxy.ansible.com в оффлайне недоступен
if $SSH 'sudo test -d /root/.ansible/collections/ansible_collections/community'; then
  echo "== ansible-коллекции из VM"
  $SSH 'sudo tar czf /tmp/ansible-collections.tar.gz -C /root .ansible'
  $SCP megapolos@localhost:/tmp/ansible-collections.tar.gz "$BUNDLE/ansible-collections.tar.gz"
  $SSH 'sudo rm -f /tmp/ansible-collections.tar.gz'
else
  echo "WARN: в VM нет /root/.ansible — bootstrap в оффлайне не сможет INIT ноды" >&2
fi

# docker-образ registry:2 (INSTALL REGISTRY тянет его с hub.docker.com)
mkdir -p "$BUNDLE/docker"
if $SSH 'sudo docker image inspect registry:2 >/dev/null 2>&1'; then
  echo "== docker-образ registry:2 из VM"
  $SSH 'sudo docker save registry:2 | gzip' > "$BUNDLE/docker/registry-2.tar.gz"
else
  echo "WARN: в VM нет образа registry:2 — прогони онлайн-установку с bootstrap" >&2
fi

echo "== apt-пакеты из VM (/var/cache/apt/archives)"
$SSH 'sudo bash -c "cd /var/cache/apt/archives && tar czf /tmp/apt-debs.tar.gz --exclude=lock --exclude=partial ."'
$SCP megapolos@localhost:/tmp/apt-debs.tar.gz /tmp/megapolos-apt-debs.tar.gz
$SSH 'sudo rm -f /tmp/apt-debs.tar.gz'
tar xzf /tmp/megapolos-apt-debs.tar.gz -C "$BUNDLE/debs"
rm -f /tmp/megapolos-apt-debs.tar.gz
# битые частичные загрузки не нужны
find "$BUNDLE/debs" -name '*.deb' -size -1k -delete

echo "== git-репозитории из workspace"
SRC_WS="$(cd "$WORKSPACE/.." && pwd)/megapolos"   # клоны core/gui живут в соседнем каталоге
for r in megapolos-core megapolos-gui; do
  git -C "$SRC_WS/$r" update-server-info
  git clone --quiet "file://$SRC_WS/$r/.git" "$BUNDLE/repos/$r.git"
  git -C "$BUNDLE/repos/$r.git" update-server-info
done

echo "== дамп БД и установщики"
cp "$SRC_WS/megapolos-core/install/newpostgresql.sql" "$BUNDLE/pg/"
cp "$WORKSPACE/install/megapolos-install.sh" "$BUNDLE/install.sh"
chmod +x "$BUNDLE/install.sh"
# Go-установщик (основной); собирается в контейнере golang
if [ ! -x "$WORKSPACE/megapolos-installer" ]; then
  echo "== собираю installer ($ENGINE golang)"
  "$ENGINE" run --rm -v "$WORKSPACE:/src:Z" -v "$WORKSPACE/vm/cache/gomod:/go/pkg/mod:Z" \
    -w /src docker.io/library/golang:1.24-bookworm bash -c \
    'CGO_ENABLED=0 go build -o megapolos-installer .'
fi
cp "$WORKSPACE/megapolos-installer" "$BUNDLE/installer"
chmod +x "$BUNDLE/installer"

echo "== package-lock.json из VM (npm --offline требует lock; в git их нет)"
$SCP megapolos@localhost:/opt/megapolos/megapolos-core/package-lock.json "$BUNDLE/package-lock.core.json"
$SCP megapolos@localhost:/opt/megapolos/megapolos-gui/package-lock.json "$BUNDLE/package-lock.gui.json"

CORE_VER=$(git -C "$SRC_WS/megapolos-core" log -1 --format='%h %ci' || echo unknown)
GUI_VER=$(git -C "$SRC_WS/megapolos-gui" log -1 --format='%h %ci' || echo unknown)
cat > "$BUNDLE/INFO.txt" <<EOF
Megapolos offline bundle (squashfs)
Собран: $(date -Iseconds) из VM :$PORT
megapolos-core: $CORE_VER
megapolos-gui:  $GUI_VER
Целевая ОС: Ubuntu 24.04 / Debian 12 (amd64)

Установка на чистую машину:
  sudo mount -o ro,loop megapolos-bundle.sqfs /mnt
  sudo /mnt/installer            # TUI; или --no-tui --yes для headless
EOF

echo "== пакую bundle/megapolos-bundle.sqfs (zstd)"
# squashfs монтируется ядром без распаковки: скачал → mount → installer
rm -f "$WORKSPACE/megapolos-bundle.sqfs"
mksquashfs "$BUNDLE" "$WORKSPACE/megapolos-bundle.sqfs" -comp zstd -noappend -quiet
mv "$WORKSPACE/megapolos-bundle.sqfs" "$BUNDLE/megapolos-bundle.sqfs"

du -sh "$BUNDLE"/* | sort -h
echo "ГОТОВО: $BUNDLE (и megapolos-bundle.sqfs внутри)"
