#!/usr/bin/env bash
# =============================================================================
# megapolos-install.sh — установка Megapolos (megapolos-core + megapolos-gui)
#
# Запускается ВНУТРИ виртуальной машины (Ubuntu 24.04 / Debian 12).
#
#   curl -fsSL http://<host>:8000/install/megapolos-install.sh | sudo bash
#   sudo bash megapolos-install.sh                 # из файла
#   sudo MEGAPOLOS_INTERACTIVE=0 bash megapolos-install.sh   # без вопросов
#
# Режимы:
#   * интерактивный TUI (ветка, домен, параметры) — если есть терминал
#   * оффлайн — автоматически, если на хосте 10.0.2.2 подняты кэши
#     (apt-cacher :3142, verdaccio :4873, зеркало :8000 — см. vm/host-services.sh)
#   * бандл — если задан MEGAPOLOS_BUNDLE_DIR с debs/npm-cache/repos
#
# Переменные окружения (все опциональны, перекрывают вопросы TUI):
#   MEGAPOLOS_GIT_BASE    — базовый URL/путь репозиториев (auto по умолчанию)
#   MEGAPOLOS_CORE_REF    — ветка/тег core (main)
#   MEGAPOLOS_GUI_REF     — ветка/тег gui (main)
#   MEGAPOLOS_API_URL     — URL API для GUI (http://localhost:5100)
#   MEGAPOLOS_DEV_MODE    — true/false (true)
#   MEGAPOLOS_DEBUG       — true/false (false)
#   MEGAPOLOS_DB_NAME     — имя БД (megapolos)
#   MEGAPOLOS_DB_USER     — юзер БД (megapolos)
#   MEGAPOLOS_DIR         — куда ставить (/opt/megapolos)
#   MEGAPOLOS_BUNDLE_DIR  — каталог оффлайн-бандла (debs/, npm-cache.tar.gz, repos/)
#   MEGAPOLOS_ADD_SELF_NODE   — true/false/auto: добавить этот хост как ноду (auto)
#   MEGAPOLOS_NODE_ROOT_PASSWORD — пароль root для SSH себя-ноды (megapolos)
#   MEGAPOLOS_INTERACTIVE — 0 отключает вопросы
#
# Скрипт идемпотентен: повторный запуск пропускает сделанное.
# =============================================================================
set -euo pipefail

# ---------------- параметры по умолчанию ----------------
GIT_BASE="${MEGAPOLOS_GIT_BASE:-auto}"
CORE_REF="${MEGAPOLOS_CORE_REF:-main}"
GUI_REF="${MEGAPOLOS_GUI_REF:-main}"
API_URL="${MEGAPOLOS_API_URL:-http://localhost:5100}"
DEV_MODE="${MEGAPOLOS_DEV_MODE:-true}"
DEBUG="${MEGAPOLOS_DEBUG:-false}"
DB_NAME="${MEGAPOLOS_DB_NAME:-megapolos}"
DB_USER="${MEGAPOLOS_DB_USER:-megapolos}"
INSTALL_DIR="${MEGAPOLOS_DIR:-/opt/megapolos}"
BUNDLE_DIR="${MEGAPOLOS_BUNDLE_DIR:-}"
# автоопределение бандла: рядом со скриптом или в PWD лежат debs/ + npm cache
if [ -z "$BUNDLE_DIR" ]; then
  for d in "$PWD" "$(cd "$(dirname "$0")" 2>/dev/null && pwd || true)"; do
    [ -n "$d" ] && [ -d "$d/debs" ] && { BUNDLE_DIR="$d"; break; }
  done
fi
SVC_USER=megapolos
ADD_SELF_NODE="${MEGAPOLOS_ADD_SELF_NODE:-auto}"   # true/false/auto
NODE_ROOT_PASSWORD="${MEGAPOLOS_NODE_ROOT_PASSWORD:-megapolos}"
HOST_IP=10.0.2.2
LOCAL_GIT_BASE="http://$HOST_IP:8000"
GITLAB_BASE="https://gitlab.com/megapolos"
APT_PROXY=""   # заполняется при автоопределении
NPM_REGISTRY=""
PG_MAJOR=16
NODE_MAJOR=18

# ---------------- вывод / UI ----------------
cG=$'\033[1;32m'; cY=$'\033[1;33m'; cR=$'\033[1;31m'; cC=$'\033[1;36m'; c0=$'\033[0m'
log()  { printf '\n%s==> [%s] %s%s\n' "$cG" "$(date +%H:%M:%S)" "$*" "$c0"; }
warn() { printf '%s==> WARN: %s%s\n' "$cY" "$*" "$c0" >&2; }
die()  { printf '%s==> FAIL: %s%s\n' "$cR" "$*" "$c0" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "Запусти от root: sudo bash $0"
command -v apt-get >/dev/null || die "Нужен apt (Debian/Ubuntu)"

HAVE_TTY=0
# /dev/tty существует и без controlling terminal (0666) — проверяем реальным открытием
if [ "${MEGAPOLOS_INTERACTIVE:-1}" != "0" ] && { true < /dev/tty; } 2>/dev/null; then HAVE_TTY=1; fi
HAVE_WHIPTAIL=0
command -v whiptail >/dev/null 2>&1 && HAVE_WHIPTAIL=1

# ввод (всё через /dev/tty — работает и при curl|bash)
ui_ask() { # $1=вопрос $2=default -> echo ответ
  local ans
  if [ "$HAVE_WHIPTAIL" -eq 1 ]; then
    ans="$(whiptail --title "Megapolos" --inputbox "$1" 10 70 "$2" 3>&1 1>&2 2>&3 </dev/tty >/dev/tty)" || die "отменено"
  else
    printf '%s?%s [%s]: ' "$cC$1" "$c0" "$2" > /dev/tty
    read -r ans < /dev/tty
  fi
  echo "${ans:-$2}"
}
ui_yesno() { # $1=вопрос $2=default(yes/no)
  if [ "$HAVE_WHIPTAIL" -eq 1 ]; then
    local args=(); [ "$2" = "no" ] && args=(--defaultno)
    whiptail --title "Megapolos" --yesno "$1" 8 70 "${args[@]}" </dev/tty >/dev/tty 2>&1 \
      && { echo true; return; } || { echo false; return; }
  fi
  local ans
  printf '%s? [%s]: ' "$cC$1" "$c0" "$([ "$2" = yes ] && echo 'Y/n' || echo 'y/N')" > /dev/tty
  read -r ans < /dev/tty
  ans="${ans:-$2}"; case "$ans" in y|Y|yes|да|Д) echo true;; *) echo false;; esac
}
ui_menu() { # $1=заголовок; далее опции; echo выбранная
  local title="$1"; shift
  if [ "$HAVE_WHIPTAIL" -eq 1 ]; then
    local items=() i=1 o
    for o in "$@"; do items+=("$i" "$o"); i=$((i+1)); done
    local pick
    pick="$(whiptail --title "Megapolos" --menu "$title" 20 70 12 "${items[@]}" 3>&1 1>&2 2>&3 </dev/tty >/dev/tty)" || die "отменено"
    i=1; for o in "$@"; do [ "$i" = "$pick" ] && { echo "$o"; return; }; i=$((i+1)); done
  fi
  {
    printf '%s%s:%s\n' "$cC" "$title" "$c0"
    local i=1 o
    for o in "$@"; do printf '  %d) %s\n' "$i" "$o"; i=$((i+1)); done
    printf 'номер [1]: '
  } > /dev/tty
  local ans; read -r ans < /dev/tty; ans="${ans:-1}"
  local i=1
  for o in "$@"; do [ "$i" = "$ans" ] && { echo "$o"; return; }; i=$((i+1)); done
  echo "$1"
}

# ---------------- автоопределение оффлайн-кэшей ----------------
# живость = любой HTTP-ответ (apt-cacher-ng на / отвечает 404 — это норма)
probe() { curl -s --max-time 2 -o /dev/null "$1"; }
if probe "http://$HOST_IP:3142";  then APT_PROXY="http://$HOST_IP:3142"; fi
if probe "http://$HOST_IP:4873";  then NPM_REGISTRY="http://$HOST_IP:4873"; fi

export DEBIAN_FRONTEND=noninteractive
APT="apt-get -y -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold"

. /etc/os-release
log "Дистрибутив: $PRETTY_NAME"
case "$ID" in ubuntu|debian) ;; *) warn "дистрибутив не тестировался";; esac

# ---------------- бандл: локальные debs ----------------
if [ -n "$BUNDLE_DIR" ] && [ -d "$BUNDLE_DIR/debs" ]; then
  log "Бандл-режим: ставлю пакеты из $BUNDLE_DIR/debs (dpkg, без сети)"
  # dpkg ставит ровно те версии, что в бандле (apt предпочитал бы новые из репо и лез в сеть)
  dpkg -i "$BUNDLE_DIR"/debs/*.deb >/dev/null 2>&1 || true
  $APT install -f || warn "dpkg: не все зависимости разрешились из бандла"
fi

# ---------------- 1. базовые пакеты + whiptail для TUI ----------------
log "Шаг 1/10: базовые пакеты"
[ -n "$APT_PROXY" ] && { printf 'Acquire::http::Proxy "%s";\nAcquire::http::Timeout "15";\nAcquire::https::Timeout "15";\nAcquire::Retries "1";\n' "$APT_PROXY" > /etc/apt/apt.conf.d/99megapolos-proxy; log "apt через кэш $APT_PROXY"; }
apt-get update || warn "apt update: репозитории недоступны — продолжаю на бандле/кэше"
$APT install curl ca-certificates gnupg lsb-release git build-essential python3 nginx whiptail \
  || $APT install curl ca-certificates gnupg lsb-release git build-essential python3 nginx
command -v whiptail >/dev/null 2>&1 && HAVE_WHIPTAIL=1

# ---------------- 2. TUI-опрос ----------------
if [ "$HAVE_TTY" -eq 1 ]; then
  log "Настройка установки (Enter = значение по умолчанию)"
  # источник кода
  if [ "$GIT_BASE" = "auto" ]; then
    if probe "$LOCAL_GIT_BASE/"; then GIT_BASE="$LOCAL_GIT_BASE"; else GIT_BASE="$GITLAB_BASE"; fi
    if [ "$GIT_BASE" = "$LOCAL_GIT_BASE" ]; then
      choice="$(ui_menu "Откуда брать репозитории" "локальное зеркало $LOCAL_GIT_BASE (найдено)" "gitlab.com $GITLAB_BASE")"
    else
      choice="$(ui_menu "Откуда брать репозитории" "gitlab.com $GITLAB_BASE" "локальное зеркало $LOCAL_GIT_BASE")"
    fi
    case "$choice" in локальное*) GIT_BASE="$LOCAL_GIT_BASE";; *) GIT_BASE="$GITLAB_BASE";; esac
  fi
  src_url() { if [ "${GIT_BASE#http://$HOST_IP}" != "$GIT_BASE" ]; then echo "$GIT_BASE/$1/.git"; else echo "$GIT_BASE/$1.git"; fi; }
  # ветки
  CORE_REF="$(ui_ask "Ветка/тег megapolos-core" "$CORE_REF")"
  GUI_REF="$(ui_ask "Ветка/тег megapolos-gui" "$GUI_REF")"
  API_URL="$(ui_ask "URL API для GUI (домен)" "$API_URL")"
  DEV_MODE="$(ui_yesno "devMode (все контейнеры на localhost)" "$([ "$DEV_MODE" = true ] && echo yes || echo no)")"
  DEBUG="$(ui_yesno "debug-логи" "$([ "$DEBUG" = true ] && echo yes || echo no)")"
  DB_NAME="$(ui_ask "Имя базы данных" "$DB_NAME")"
  DB_USER="$(ui_ask "Пользователь БД" "$DB_USER")"
  [ "$ADD_SELF_NODE" = "auto" ] && ADD_SELF_NODE="$(ui_yesno "Добавить этот хост как ноду megapolos (root@127.0.0.1:22)?" yes)"
  summary="Источник:  $GIT_BASE
core:      $CORE_REF
gui:       $GUI_REF
API URL:   $API_URL
devMode:   $DEV_MODE   debug: $DEBUG
БД:        $DB_NAME (юзер $DB_USER)
Каталог:   $INSTALL_DIR
Себя в ноды: $ADD_SELF_NODE
apt-кэш:   ${APT_PROXY:-нет}   npm-реестр: ${NPM_REGISTRY:-нет}"
  [ "$(ui_yesno "$summary

Начинаем установку?" yes)" = true ] || die "отменено пользователем"
else
  src_url() { if [ "${GIT_BASE#http://$HOST_IP}" != "$GIT_BASE" ]; then echo "$GIT_BASE/$1/.git"; else echo "$GIT_BASE/$1.git"; fi; }
  [ "$GIT_BASE" = "auto" ] && { probe "$LOCAL_GIT_BASE/" && GIT_BASE="$LOCAL_GIT_BASE" || GIT_BASE="$GITLAB_BASE"; }
  [ "$ADD_SELF_NODE" = "auto" ] && ADD_SELF_NODE=true
fi
log "Источник: $GIT_BASE (core@$CORE_REF, gui@$GUI_REF)"

# ---------------- 3. swap ----------------
log "Шаг 2/10: swap 2G (если нет)"
if ! swapon --show | grep -q .; then
  fallocate -l 2G /swapfile && chmod 600 /swapfile && mkswap /swapfile && swapon /swapfile
  grep -q '^/swapfile' /etc/fstab || echo '/swapfile none swap sw 0 0' >> /etc/fstab
else echo "swap уже есть"; fi

# ---------------- 4. Node.js ----------------
log "Шаг 3/10: Node.js $NODE_MAJOR"
node_ok() { command -v node >/dev/null && [ "$(node -v | sed 's/v\([0-9]*\).*/\1/')" -ge "$NODE_MAJOR" ]; }
if ! node_ok; then
  $APT install nodejs npm || true
  if ! node_ok; then
    log "В дистрибутиве старый node, ставлю NodeSource $NODE_MAJOR"
    curl -fsSL "https://deb.nodesource.com/setup_${NODE_MAJOR}.x" | bash -
    $APT install nodejs
  fi
fi
node_ok || die "node $NODE_MAJOR+ не установлен"
node -v; npm -v
# устойчивость к флакающей сети
npm config set fetch-retries 5
npm config set fetch-retry-mintimeout 10000
npm config set fetch-retry-maxtimeout 120000
npm config set fetch-timeout 600000
if [ -n "$NPM_REGISTRY" ]; then
  npm config set registry "$NPM_REGISTRY"; log "npm через зеркало $NPM_REGISTRY"
else
  npm config delete registry 2>/dev/null || true   # убрать зеркало с прошлых запусков
fi
NPM_OFFLINE=""
if [ -n "$BUNDLE_DIR" ]; then
  ROOT_CACHE="$BUNDLE_DIR/npm-cache-root.tar.gz"
  [ -f "$ROOT_CACHE" ] || ROOT_CACHE="$BUNDLE_DIR/npm-cache.tar.gz"   # fallback на общий кэш
  if [ -f "$ROOT_CACHE" ]; then
    log "Бандл-режим: разворачиваю npm-кэш"
    tar xzf "$ROOT_CACHE" -C /root/ 2>/dev/null || true   # внутри .npm/ → в home
    NPM_OFFLINE="--offline"
  fi
fi
npm install -g $NPM_OFFLINE nodemon ts-node

# ---------------- 5. PostgreSQL ----------------
log "Шаг 4/10: PostgreSQL $PG_MAJOR"
if ! dpkg -s "postgresql-$PG_MAJOR" >/dev/null 2>&1; then
  install -d /usr/share/keyrings
  if [ ! -f /usr/share/keyrings/pgdg.gpg ]; then
    curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc | gpg --dearmor -o /usr/share/keyrings/pgdg.gpg
  fi
  echo "deb [signed-by=/usr/share/keyrings/pgdg.gpg] http://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" \
    > /etc/apt/sources.list.d/pgdg.list
  apt-get update || warn "apt update (pgdg): нет сети — надеюсь на бандл/кэш"
  $APT install "postgresql-$PG_MAJOR"
fi
systemctl enable --now postgresql

# ---------------- 6. Docker + ansible ----------------
log "Шаг 5/10: Docker + ansible"
$APT install docker.io ansible
systemctl enable --now docker

# ---------------- 7. системный юзер ----------------
log "Шаг 6/10: пользователь $SVC_USER"
if ! id "$SVC_USER" >/dev/null 2>&1; then
  useradd -m -s /bin/bash "$SVC_USER"
fi
mkdir -p "/home/$SVC_USER"
chown "$SVC_USER:$SVC_USER" "/home/$SVC_USER"
usermod -aG docker "$SVC_USER"
# ansible-плейбуки ядра используют become: yes → нужен sudo без пароля
echo "$SVC_USER ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/90-$SVC_USER
chmod 440 /etc/sudoers.d/90-$SVC_USER
# npm-реестр и сетевые настройки для сервисного юзера (или сброс зеркала)
for kv in fetch-retries=5 fetch-retry-mintimeout=10000 fetch-retry-maxtimeout=120000 fetch-timeout=600000; do
  sudo -u "$SVC_USER" -H npm config set "${kv%%=*}" "${kv##*=}"
done
if [ -n "$NPM_REGISTRY" ]; then
  sudo -u "$SVC_USER" -H npm config set registry "$NPM_REGISTRY"
else
  sudo -u "$SVC_USER" -H npm config delete registry 2>/dev/null || true
fi
if [ -n "$BUNDLE_DIR" ] && [ -f "$BUNDLE_DIR/npm-cache.tar.gz" ]; then
  sudo -u "$SVC_USER" -H mkdir -p "/home/$SVC_USER"
  tar xzf "$BUNDLE_DIR/npm-cache.tar.gz" -C "/home/$SVC_USER/" 2>/dev/null || true   # внутри .npm/
  chown -R "$SVC_USER:$SVC_USER" "/home/$SVC_USER/.npm"
fi

# ---------------- 8. исходники ----------------
log "Шаг 7/10: исходники в $INSTALL_DIR"
mkdir -p "$INSTALL_DIR"
chown "$SVC_USER:$SVC_USER" "$INSTALL_DIR"
[ -n "$BUNDLE_DIR" ] && [ -d "$BUNDLE_DIR/repos" ] && chmod -R a+rX "$BUNDLE_DIR/repos"
as_svc() { sudo -u "$SVC_USER" -H bash -c "$*"; }
npm_retry() { # $1=каталог, остальное — аргументы npm; 3 попытки (сеть флакает)
  local dir="$1"; shift
  local i
  for i in 1 2 3; do
    as_svc "cd '$dir' && npm $*" && return 0
    warn "npm $* в $dir — попытка $i/3 неудачна, повтор через 15с"
    # прерванный npm install оставляет недокачанные пакеты в node_modules — чистим
    as_svc "rm -rf '$dir/node_modules'" || true
    sleep 15
  done
  return 1
}
clone_or_pull() { # $1=repo $2=ref
  local dir="$INSTALL_DIR/$1" url
  url="$(src_url "$1")"
  if [ -n "$BUNDLE_DIR" ] && [ -d "$BUNDLE_DIR/repos/$1.git" ]; then url="$BUNDLE_DIR/repos/$1.git"; fi
  if [ -d "$dir/.git" ]; then
    as_svc "cd '$dir' && git fetch --all --tags --prune || true"
  else
    rm -rf "$dir"
    as_svc "git clone '$url' '$dir'"
  fi
  as_svc "cd '$dir' && git checkout '$2' && (git pull --ff-only origin '$2' || true)"
}
clone_or_pull megapolos-core "$CORE_REF"
clone_or_pull megapolos-gui "$GUI_REF"
# npm --offline требует package-lock.json, а в git его нет — берём из бандла
if [ -n "$BUNDLE_DIR" ]; then
  [ ! -f "$INSTALL_DIR/megapolos-core/package-lock.json" ] && [ -f "$BUNDLE_DIR/package-lock.core.json" ] && \
    { cp "$BUNDLE_DIR/package-lock.core.json" "$INSTALL_DIR/megapolos-core/package-lock.json"; chown "$SVC_USER:$SVC_USER" "$INSTALL_DIR/megapolos-core/package-lock.json"; }
  [ ! -f "$INSTALL_DIR/megapolos-gui/package-lock.json" ] && [ -f "$BUNDLE_DIR/package-lock.gui.json" ] && \
    { cp "$BUNDLE_DIR/package-lock.gui.json" "$INSTALL_DIR/megapolos-gui/package-lock.json"; chown "$SVC_USER:$SVC_USER" "$INSTALL_DIR/megapolos-gui/package-lock.json"; }
fi

# ---------------- 9. БД ----------------
log "Шаг 8/10: база данных $DB_NAME"
DB_PASS="$(openssl rand -hex 16)"
if [ -f "$INSTALL_DIR/megapolos-core/config/config.json" ]; then
  DB_PASS="$(sed -n 's/.*"connectionString"[^:]*:[^/]*\/\/[^:]*:\([^@]*\)@.*/\1/p' \
    "$INSTALL_DIR/megapolos-core/config/config.json" || true)"
  [ -n "$DB_PASS" ] || DB_PASS="$(openssl rand -hex 16)"
fi
sudo -u postgres psql -tAc "SELECT 1 FROM pg_roles WHERE rolname='$DB_USER'" | grep -q 1 \
  || sudo -u postgres psql -c "CREATE ROLE $DB_USER SUPERUSER LOGIN PASSWORD '$DB_PASS'"
sudo -u postgres psql -c "ALTER ROLE $DB_USER PASSWORD '$DB_PASS'"
sudo -u postgres psql -tAc "SELECT 1 FROM pg_database WHERE datname='$DB_NAME'" | grep -q 1 \
  || sudo -u postgres createdb -O "$DB_USER" "$DB_NAME"
TABLES="$(sudo -u postgres psql -d "$DB_NAME" -tAc \
  "SELECT count(*) FROM information_schema.tables WHERE table_schema='public'")"
if [ "$TABLES" -eq 0 ]; then
  log "Заливаю дамп newpostgresql.sql"
  # дамп снят pg_dump 18: выкидываем \restrict/\unrestrict и SET transaction_timeout (PG17+)
  sed -e '/^\\restrict/d' -e '/^\\unrestrict/d' -e '/^SET transaction_timeout/d' \
    "$INSTALL_DIR/megapolos-core/install/newpostgresql.sql" \
    | sudo -u postgres psql -q -d "$DB_NAME" >/dev/null
  echo "Дамп загружен"
else
  echo "В БД уже $TABLES таблиц, дамп не заливаю"
fi

# ---------------- 10. core ----------------
log "Шаг 9/10: megapolos-core"
CORE_DIR="$INSTALL_DIR/megapolos-core"
if [ ! -f "$CORE_DIR/config/config.json" ]; then
  SECRET="$(openssl rand -hex 32)"
  cat > "$CORE_DIR/config/config.json" <<EOF
{
  "secret": "$SECRET",
  "connectionString": "postgres://$DB_USER:$DB_PASS@localhost:5432/$DB_NAME",
  "registryHost": "",
  "registryUser": "",
  "registryPassword": "",
  "debug": $DEBUG,
  "devMode": $DEV_MODE,
  "publicSchema": false,
  "allowUnauthorized": false,
  "noRoot": true,
  "catalogUrl": ""
}
EOF
  chown "$SVC_USER:$SVC_USER" "$CORE_DIR/config/config.json"
  echo "Создан config.json (noRoot: true)"
else
  echo "config.json уже есть, оставляю"
fi
npm_retry "$CORE_DIR" "install $NPM_OFFLINE"

# ---------------- 11. gui ----------------
log "Шаг 10/10: megapolos-gui"
GUI_DIR="$INSTALL_DIR/megapolos-gui"
printf '{\n  "server": "%s"\n}\n' "$API_URL" > "$GUI_DIR/public/config/config.json"
chown "$SVC_USER:$SVC_USER" "$GUI_DIR/public/config/config.json"
npm_retry "$GUI_DIR" "install --force $NPM_OFFLINE"
as_svc "cd '$GUI_DIR' && DISABLE_ESLINT_PLUGIN=true NODE_OPTIONS=--max-old-space-size=3072 npm run build"

# ---------------- systemd + nginx ----------------
log "systemd + nginx"
cat > /etc/systemd/system/megapolos-core.service <<EOF
[Unit]
Description=Megapolos Core (GraphQL API :5100)
After=network-online.target postgresql.service docker.service
Wants=network-online.target

[Service]
Type=simple
# Платформа делает spawn(..., uid: 0) для shell/ansible (Process.ts) —
# непривилегированному юзеру ядро возвращает EPERM. Сервис обязан быть root.
User=root
Group=root
WorkingDirectory=$CORE_DIR
ExecStart=/usr/bin/npm run prod
Restart=on-failure
RestartSec=5
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF

cat > /etc/nginx/sites-available/megapolos-gui <<EOF
server {
    listen 80 default_server;
    root $GUI_DIR/build;
    index index.html;
    client_max_body_size 10g;
    location / { try_files \$uri \$uri/ /index.html; }
}
EOF
ln -sf /etc/nginx/sites-available/megapolos-gui /etc/nginx/sites-enabled/megapolos-gui
rm -f /etc/nginx/sites-enabled/default

systemctl daemon-reload
systemctl enable --now megapolos-core
systemctl restart megapolos-core
systemctl enable --now nginx
systemctl restart nginx

# ---------------- токен ----------------
log "Жду API на :5100 и забираю root-токен"
TOKEN=""
for _ in $(seq 1 60); do
  # JWT из текущей загрузки сервиса (формат eyJ...), берём последний
  TOKEN="$(journalctl -u megapolos-core -b --no-pager 2>/dev/null | grep -oP "token: '\K(eyJ[A-Za-z0-9_.-]+)" | tail -1 || true)"
  [ -n "$TOKEN" ] && break
  sleep 3
done
if [ -n "$TOKEN" ]; then
  install -m 600 /dev/null /root/megapolos-token.txt
  printf '%s\n' "$TOKEN" > /root/megapolos-token.txt
  # копия для сервисного юзера (root-файл ему недоступен)
  SVC_HOME=$(getent passwd "$SVC_USER" | cut -d: -f6)
  if [ -n "$SVC_HOME" ] && [ -d "$SVC_HOME" ]; then
    printf '%s\n' "$TOKEN" > "$SVC_HOME/megapolos-token.txt"
    chown "$SVC_USER:$SVC_USER" "$SVC_HOME/megapolos-token.txt"
    chmod 600 "$SVC_HOME/megapolos-token.txt"
  fi
  cat > /etc/motd <<EOF

  Megapolos установлен.
  GUI:        http://<ip-vm>/
  API:        http://<ip-vm>:5100
  Root-токен: $TOKEN
  (токен также в /root/megapolos-token.txt)

EOF
else
  warn "Токен не найден за 3 минуты. Смотри: journalctl -u megapolos-core -f"
fi

# ---------------- этот хост как нода megapolos ----------------
# Платформа ходит на ноды по ssh2, порт 22 зашит в коде (ExternalProcess.ts),
# auth только по паролю (NodeInput: host/user/password). Для себя-ноды нужен
# root-вход по паролю на 127.0.0.1:22.
if [ "$ADD_SELF_NODE" = "true" ]; then
  log "Шаг 11/11: этот хост как нода megapolos (root@127.0.0.1:22)"
  echo "root:$NODE_ROOT_PASSWORD" | chpasswd
  printf 'PermitRootLogin yes\nPasswordAuthentication yes\n' > /etc/ssh/sshd_config.d/60-megapolos-root.conf
  rm -f /etc/ssh/sshd_config.d/50-cloud-init.conf /etc/ssh/sshd_config.d/60-cloudimg-settings.conf
  sshd -t && { systemctl reload ssh 2>/dev/null || systemctl reload sshd 2>/dev/null || true; }

  if [ -n "$TOKEN" ]; then
    HN="$(hostname)"
    NODES_JSON="$(curl -s -m 10 -H "token: $TOKEN" -H 'Content-Type: application/json' \
      -d '{"query":"{ getAllNode { id name } }"}' http://127.0.0.1:5100/ || true)"
    if printf '%s' "$NODES_JSON" | grep -q "\"name\":\"$HN\""; then
      log "Нода '$HN' уже существует — пропускаю"
    else
      RESP="$(curl -s -m 15 -H "token: $TOKEN" -H 'Content-Type: application/json' \
        -d "{\"query\":\"mutation CreateNode(\$values: NodeInput!) { createNode(values: \$values) { id name autoCreateInstances } }\",\"variables\":{\"values\":{\"name\":\"$HN\",\"host\":\"127.0.0.1\",\"user\":\"root\",\"password\":\"$NODE_ROOT_PASSWORD\",\"autoCreateInstances\":true}}}" \
        http://127.0.0.1:5100/ || true)"
      if printf '%s' "$RESP" | grep -q '"createNode":{"id"'; then
        log "Нода '$HN' добавлена (autoCreateInstances=true)"
      else
        warn "Не удалось добавить ноду. Ответ API: $RESP"
      fi
    fi
  else
    warn "Нет токена — ноду добавить не могу (добавь вручную в GUI: host 127.0.0.1, user root)"
  fi
fi

log "ГОТОВО"
echo "  GUI:   http://<ip-vm>/"
echo "  API:   http://<ip-vm>:5100"
echo "  Токен: /root/megapolos-token.txt"
[ "$ADD_SELF_NODE" = "true" ] && echo "  Нода:  $(hostname) → 127.0.0.1 (root / $NODE_ROOT_PASSWORD)"
echo "  Логи:  journalctl -u megapolos-core -f"
