#!/usr/bin/env bash
# =============================================================================
# make-bundle-docker.sh — воспроизводимая сборка megapolos-bundle в контейнере
# (podman в приоритете, docker как fallback), БЕЗ установленной VM-донора.
#
#   ./vm/make-bundle-docker.sh [24.04|22.04]    (по умолчанию 24.04)
#
#   1. собирает бинарь установщика (golang-контейнер)
#   2. готовит контекст vm/run/bundle-ctx/ (клоны репо из ../megapolos,
#      docker-образы из bundle/docker, бинарь, bootstrap.sh)
#   3. build bundle/Dockerfile → megapolos-bundle-<версия>.sqfs в bundle/
#
# Старый путь (из живой VM) — vm/make-bundle.sh — остаётся как fallback.
# =============================================================================
set -euo pipefail
WS="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$WS"

VER="${1:-24.04}"
case "$VER" in 24.04|22.04) ;; *) echo "поддерживается: 24.04 | 22.04" >&2; exit 1;; esac
SRC_WS="$(cd "$WS/.." && pwd)/megapolos"   # клоны core/gui — соседний каталог
CTX="vm/run/bundle-ctx"
OUT="megapolos-bundle-$VER.sqfs"

ENGINE="$(command -v podman || command -v docker || true)"
[ -n "$ENGINE" ] || { echo "нужен podman или docker" >&2; exit 1; }

for r in megapolos-core megapolos-gui; do
  [ -d "$SRC_WS/$r/.git" ] || { echo "нет клона $SRC_WS/$r" >&2; exit 1; }
done

echo "== 1/4: бинарь установщика (ubuntu $VER)"
$ENGINE run --rm -v "$WS:/src:Z" -v "$WS/vm/cache/gomod:/go/pkg/mod:Z" -w /src \
  golang:1.24-bookworm bash -c 'CGO_ENABLED=0 go build -o megapolos-installer .'

echo "== 2/4: контекст сборки ($CTX)"
rm -rf "$CTX"; mkdir -p "$CTX"
for r in megapolos-core megapolos-gui; do
  git clone --quiet "$SRC_WS/$r" "$CTX/$r"
done
cp -a bundle/docker "$CTX/docker" 2>/dev/null || mkdir -p "$CTX/docker"
# ansible-ассеты для старых ОС (jammy: ansible 2.10 в репо) — venv wheels + коллекции
if [ ! -d bundle/pip ] || [ ! -d bundle/ansible-collections ]; then
  echo "== ansible-ассеты отсутствуют — скачиваю (vm/fetch-ansible-assets.sh)"
  ./vm/fetch-ansible-assets.sh
fi
cp -a bundle/pip "$CTX/pip"
cp -a bundle/ansible-collections "$CTX/ansible-collections"
cp megapolos-installer "$CTX/installer"
cp install/bootstrap.sh "$CTX/install.sh"
cp bundle/Dockerfile "$CTX/Dockerfile"

echo "== 3/4: $ENGINE build (ubuntu $VER)"
$ENGINE build --build-arg "UBUNTU=$VER" -t "megapolos-bundle-builder:$VER" "$CTX"

echo "== 4/4: забираю $OUT"
id="$($ENGINE create "megapolos-bundle-builder:$VER")"
trap "$ENGINE rm $id >/dev/null 2>&1 || true" EXIT
$ENGINE cp "$id:/bundle/$OUT" "bundle/$OUT"

ls -la "bundle/$OUT"
echo "ГОТОВО: bundle/$OUT"
