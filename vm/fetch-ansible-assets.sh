#!/usr/bin/env bash
# =============================================================================
# fetch-ansible-assets.sh — скачать ansible-ассеты для бандла:
#   bundle/pip/*.whl                  — ansible-core 2.16 (+deps) для старых ОС
#                                       (jammy тащит ansible 2.10 из репо —
#                                       плейбуки платформы требуют >= 2.14)
#   bundle/ansible-collections/*.tar.gz — community.{general,docker,crypto}
#
# Запуск на хосте (нужен podman/docker + сеть). Идемпотентно.
# =============================================================================
set -euo pipefail
cd "$(dirname "$0")/.."
W="$PWD"
ENGINE="$(command -v podman || command -v docker || true)"
[ -n "$ENGINE" ] || { echo "нужен podman или docker" >&2; exit 1; }

ANSIBLE_CORE_VER="2.16.14"
mkdir -p bundle/pip bundle/ansible-collections

echo "== pip wheels: ansible-core==$ANSIBLE_CORE_VER (py3.10-совместимые)"
"$ENGINE" run --rm -v "$W/bundle/pip:/out:Z" docker.io/library/python:3.10-slim \
  pip download --no-cache-dir -d /out "ansible-core==$ANSIBLE_CORE_VER" pip setuptools wheel

echo "== galaxy collections (версии, совместимые с ansible-core $ANSIBLE_CORE_VER)"
# highest_version с galaxy может требовать более новый core — пиним проверенные
dl() { # $1 = ns.name, $2 = версия
  local tar="bundle/ansible-collections/$1-$2.tar.gz"
  rm -f bundle/ansible-collections/$1-*.tar.gz  # одна версия на коллекцию
  [ -f "$tar" ] && { echo "  $1 $2 уже есть"; return; }
  echo "  $1 $2"
  local ns="${1%%.*}" name="${1##*.}"
  curl -fsSL -o "$tar" "https://galaxy.ansible.com/download/${ns}-${name}-${2}.tar.gz"
}
dl community.general 9.5.8
dl community.docker 4.3.1
dl community.crypto 2.22.3

echo "ГОТОВО: bundle/pip ($(ls bundle/pip | wc -l) wheels), bundle/ansible-collections ($(ls bundle/ansible-collections | wc -l) коллекций)"
