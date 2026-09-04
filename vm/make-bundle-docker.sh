#!/usr/bin/env bash
# =============================================================================
# make-bundle-docker.sh — воспроизводимая сборка megapolos-bundle.sqfs
# в контейнере (podman/docker), БЕЗ установленной VM-донора.
#
#   ./vm/make-bundle-docker.sh
#
# Что делает:
#   1. собирает свежий бинарь установщика (golang-контейнер)
#   2. podman build bundle/Dockerfile — ubuntu:24.04: apt-debs, npm-кэши,
#      node-gyp headers, репозитории, дамп БД, lock-файлы → mksquashfs
#   3. забирает megapolos-bundle.sqfs в bundle/
#
# Старый путь (из живой VM) — vm/make-bundle.sh — остаётся как fallback.
# =============================================================================
set -euo pipefail
WS="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$WS"

ENGINE="$(command -v podman || command -v docker)"
[ -n "$ENGINE" ] || { echo "нужен podman или docker" >&2; exit 1; }

echo "== 1/3: бинарь установщика (golang-контейнер)"
$ENGINE run --rm \
  -v ./installer:/src:Z -v ./vm/cache/gomod:/go/pkg/mod:Z -w /src \
  golang:1.24-bookworm bash -c 'CGO_ENABLED=0 go build -o megapolos-installer .'

echo "== 2/3: сборка бандла (bundle/Dockerfile)"
$ENGINE build -f bundle/Dockerfile -t megapolos-bundle-builder .

echo "== 3/3: забираю megapolos-bundle.sqfs"
id="$($ENGINE create megapolos-bundle-builder)"
trap "$ENGINE rm $id >/dev/null 2>&1 || true" EXIT
$ENGINE cp "$id:/bundle/megapolos-bundle.sqfs" bundle/megapolos-bundle.sqfs

ls -la bundle/megapolos-bundle.sqfs
echo "ГОТОВО"
