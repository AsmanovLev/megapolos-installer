#!/usr/bin/env bash
# =============================================================================
# megapolos-installer install script — curl -fsSL https://install.megapolos.dev | bash
#
# Downloads the latest release binary from GitHub and runs it.
# Supports: Linux x86_64 (amd64)
# =============================================================================
set -euo pipefail

# RESULT используется после запуска установщика; инициализируем, чтобы при
# любом сбое не получить «RESULT: unbound variable» под set -u.
RESULT=0

REPO="AsmanovLev/megapolos-installer"
INSTALLER_URL_BASE="${MEGAPOLOS_INSTALLER_URL_BASE:-https://github.com/${REPO}/releases/download}"
LATEST_URL="https://api.github.com/repos/${REPO}/releases/latest"

# Colors
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'

info()    { echo -e "${GREEN}[INFO]${NC} $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC} $*"; }
err()     { echo -e "${RED}[ERROR]${NC} $*" >&2; exit 1; }

# wrap_cmd — переносит длинную команду по словам с shell-продолжением "\",
# чтобы строка не уезжала за край терминала. Ширина: $COLUMNS / tput / 100.
wrap_cmd() {
  local cmd="$1" width="${COLUMNS:-}" line="  " word
  if [[ -z "$width" ]] && command -v tput &>/dev/null; then
    width="$(tput cols 2>/dev/null || true)"
  fi
  if [[ -z "$width" || "$width" -lt 24 ]]; then
    width=100
  fi
  # shellcheck disable=SC2086  # намеренный word-splitting по словам команды
  for word in $cmd; do
    if (( ${#line} + 1 + ${#word} + 2 > width )); then
      printf '%s \\\n' "$line"
      line="  $word"
    else
      line="$line $word"
    fi
  done
  printf '%s\n' "$line"
}

# Detect arch
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64)  ARCH_TAG="amd64" ;;
  aarch64)  ARCH_TAG="arm64" ;;
  *)        err "Unsupported arch: $ARCH (supported: x86_64, aarch64)" ;;
esac

# Detect if running as root
if [[ $EUID -ne 0 ]]; then
  err "Run as root: sudo $0"
fi

# Detect if apt exists
if ! command -v apt-get &>/dev/null; then
  err "This installer requires Debian/Ubuntu (apt-get)"
fi

info "Megapolos Installer (latest)"
info "Arch: $ARCH_TAG"

# Fetch latest release tag
info "Fetching latest release info..."
RELEASE_JSON=$(curl -fsSL --max-time 30 --retry 3 --retry-all-errors --retry-delay 2 "$LATEST_URL") \
  || err "Failed to fetch releases from GitHub (check internet)"

TAG=$(echo "$RELEASE_JSON" | python3 -c "
import sys, json
data = json.load(sys.stdin)
print(data.get('tag_name',''))
" 2>/dev/null) \
  || err "Failed to parse release info"

if [[ -z "$TAG" ]]; then
  err "Could not determine latest release tag"
fi

info "Latest release: $TAG"

# Download binary
BIN_NAME="megapolos-installer"
TMP_DIR=$(mktemp -d)
trap "rm -rf $TMP_DIR" EXIT

info "Downloading ${BIN_NAME} for ${ARCH_TAG}..."
ASSET_URL="${INSTALLER_URL_BASE}/${TAG}/${BIN_NAME}"
DEST="${TMP_DIR}/${BIN_NAME}"

curl -fsSL --max-time 120 --retry 3 --retry-all-errors --retry-delay 2 -o "$DEST" "$ASSET_URL" \
  || err "Failed to download $ASSET_URL"

# Verify it's a binary
if ! file "$DEST" | grep -q "ELF"; then
  err "Downloaded file is not an ELF binary — bad release asset?"
fi

# Verify binary checksum (install.sh is verified by HTTPS transport)
info "Verifying checksum..."
CHECKSUM_URL="${INSTALLER_URL_BASE}/${TAG}/SHA256SUMS.txt"
CHECKSUM_FILE="${TMP_DIR}/SHA256SUMS.txt"
curl -fsSL --max-time 30 --retry 3 --retry-all-errors --retry-delay 2 -o "$CHECKSUM_FILE" "$CHECKSUM_URL" \
  || err "Failed to download checksums"
EXPECTED=$(grep "megapolos-installer$" "$CHECKSUM_FILE" | awk '{print $1}')
ACTUAL=$(sha256sum "$DEST" | awk '{print $1}')
if [[ "$EXPECTED" != "$ACTUAL" ]]; then
  err "Checksum mismatch — possible corrupted download"
fi
info "Checksum verified"

chmod +x "$DEST"
info "Downloaded successfully ($(du -h "$DEST" | cut -f1))"

# Авто-детект существующей установки: если на хосте уже есть /opt/megapolos
# или маркеры стадий — подсказываем --resume, чтобы повторный curl|bash
# долечивал, а не начинал с нуля. Юзер может перебить через явный флаг.
AUTO_FLAGS=""
has_existing=0
if [[ -d /opt/megapolos ]]; then
  has_existing=1
fi
if [[ -d /var/lib/megapolos ]] && ls /var/lib/megapolos/stage-*.done 2>/dev/null | grep -q .; then
  has_existing=1
fi
if [[ $has_existing -eq 1 ]]; then
  user_passed_resume=0
  user_passed_wipe=0
  for arg in "$@"; do
    case "$arg" in
      --resume|--resume=*) user_passed_resume=1 ;;
      --wipe|--wipe=*|--reset-db|--reset-db=*) user_passed_wipe=1 ;;
    esac
  done
  if [[ $user_passed_resume -eq 0 && $user_passed_wipe -eq 0 ]]; then
    AUTO_FLAGS="--resume"
    export MEGAPOLOS_AUTO_RESUME=1
    warn "На хосте найдена существующая установка (или её следы)."
    warn "Автоматически добавляю --resume (долечить, а не переустанавливать)."
    warn "Если хотите начать с нуля — добавьте --wipe."
  fi
fi

# Собираем итоговую команду для вывода
CMD="sudo $DEST $* ${AUTO_FLAGS}"
info "Running installer..."
set +e
# Важно: скрипт может исполняться через `curl | bash` — тогда stdin (fd0) это
# сам скрипт. Если запустить установщик с наследованием stdin, он вычитает
# остаток скрипта → `RESULT: unbound variable`. Поэтому stdin — с терминала
# (интерактивные вопросы) или /dev/null.
if [[ -r /dev/tty ]]; then
  "$DEST" "$@" ${AUTO_FLAGS} </dev/tty
else
  "$DEST" "$@" ${AUTO_FLAGS} </dev/null
fi
RESULT=$?
set -e

echo ""
if [[ $RESULT -eq 0 ]]; then
  info "Installation successful!"
  info "To reproduce this exact installation:"
  wrap_cmd "$CMD"
  echo ""
else
  err "Installation failed (exit $RESULT)"
  echo ""
  info "Диагностика: sudo $DEST --doctor"
  info "Логи ansible: sudo $DEST --info"
  info "Возобновить: sudo $DEST --resume (или --retry-stage=<init|prepare-for-core|install-registry>)"
fi
exit $RESULT
