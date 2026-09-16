#!/usr/bin/env bash
# =============================================================================
# megapolos-installer install script — curl -fsSL https://install.megapolos.dev | bash
#
# Downloads the latest release binary from GitHub and runs it.
# Supports: Linux x86_64 (amd64)
# =============================================================================
set -euo pipefail

REPO="AsmanovLev/megapolos-installer"
INSTALLER_URL_BASE="https://github.com/${REPO}/releases/download"
LATEST_URL="https://api.github.com/repos/${REPO}/releases/latest"

# Colors
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'

info()    { echo -e "${GREEN}[INFO]${NC} $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC} $*"; }
err()     { echo -e "${RED}[ERROR]${NC} $*" >&2; exit 1; }

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
RELEASE_JSON=$(curl -fsSL --max-time 30 "$LATEST_URL") \
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

curl -fsSL --max-time 120 -o "$DEST" "$ASSET_URL" \
  || err "Failed to download $ASSET_URL"

# Verify it's a binary
if ! file "$DEST" | grep -q "ELF"; then
  err "Downloaded file is not an ELF binary — bad release asset?"
fi

# Verify checksum
info "Verifying checksum..."
CHECKSUM_URL="${INSTALLER_URL_BASE}/${TAG}/SHA256SUMS.txt"
CHECKSUM_FILE="${TMP_DIR}/SHA256SUMS.txt"
curl -fsSL --max-time 30 -o "$CHECKSUM_FILE" "$CHECKSUM_URL" \
  || err "Failed to download checksums"
cd "$TMP_DIR" && sha256sum --status -c SHA256SUMS.txt \
  || err "Checksum mismatch — possible corrupted download"
cd - >/dev/null
info "Checksum verified"

chmod +x "$DEST"
info "Downloaded successfully ($(du -h "$DEST" | cut -f1))"

# Run the installer
info "Running installer..."
exec "$DEST" "$@"
