# =============================================================================
# megapolos-bundle — воспроизводимая сборка оффлайн-бандла в контейнере.
#
# Раньше бандл собирался из живой установленной VM (make-bundle.sh) — теперь
# всё собирается в ubuntu:24.04 контейнере из текущего состояния workspace:
#   debs (apt), npm-кэши (core+gui+globals), node-gyp headers, репозитории,
#   дамп БД, package-lock'и, бинарь установщика → megapolos-bundle.sqfs
#
# Сборка (из корня workspace):
#   ./vm/make-bundle-docker.sh
# или вручную:
#   podman build -f bundle/Dockerfile -t megapolos-bundle-builder .
#   podman create --name bb megapolos-bundle-builder
#   podman cp bb:/bundle/megapolos-bundle.sqfs bundle/
#   podman rm bb
# =============================================================================
FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive
# официальный образ чистит /var/cache/apt/archives — нам debs нужны!
RUN rm -f /etc/apt/apt.conf.d/docker-clean \
 && printf '#!/bin/sh\nexit 101\n' > /usr/sbin/policy-rc.d && chmod +x /usr/sbin/policy-rc.d

# ---------- 1. apt: весь набор пакетов установщика (debs копятся в кэше) ----------
RUN apt-get update && apt-get install -y --no-install-recommends \
      curl ca-certificates gnupg lsb-release git build-essential python3 \
      nginx openssl whiptail squashfs-tools \
      nodejs npm docker.io ansible \
 && install -d /usr/share/keyrings \
 && curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc | gpg --dearmor -o /usr/share/keyrings/pgdg.gpg \
 && echo "deb [signed-by=/usr/share/keyrings/pgdg.gpg] http://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" \
      > /etc/apt/sources.list.d/pgdg.list \
 && apt-get update && apt-get install -y postgresql-16

# npm: устойчивость к флакающей сети (как в установщике)
RUN npm config set fetch-retries 5 \
 && npm config set fetch-retry-mintimeout 10000 \
 && npm config set fetch-retry-maxtimeout 120000 \
 && npm config set fetch-timeout 600000

# ---------- 2. npm-кэш root для глобальных пакетов (маленький тарбол) ----------
RUN npm install -g nodemon ts-node \
 && mkdir -p /out \
 && tar czf /out/npm-cache-root.tar.gz -C /root .npm

# ---------- 3. исходники → npm-кэш проектов + package-lock + node-gyp ----------
COPY megapolos-core /src/megapolos-core
COPY megapolos-gui  /src/megapolos-gui

RUN cd /src/megapolos-core && npm install \
 && cd /src/megapolos-gui  && npm install --force \
 && tar czf /out/npm-cache.tar.gz -C /root .npm \
 && tar czf /out/node-gyp-cache.tar.gz -C /root .cache/node-gyp \
 && cp /src/megapolos-core/package-lock.json /out/package-lock.core.json \
 && cp /src/megapolos-gui/package-lock.json  /out/package-lock.gui.json

# ---------- 4. репозитории (file:// клоны), дамп БД, установщик ----------
# .git остаётся → git clone из бандла работает; node_modules из репо выкидываем
# (появились после npm install на шаге 3 — в бандле они не нужны, кэш npm есть)
RUN mkdir -p /out/repos \
 && cp -a /src/megapolos-core /out/repos/megapolos-core.git \
 && cp -a /src/megapolos-gui  /out/repos/megapolos-gui.git \
 && rm -rf /out/repos/megapolos-core.git/node_modules \
          /out/repos/megapolos-gui.git/node_modules \
          /out/repos/megapolos-core.git/build \
          /out/repos/megapolos-gui.git/build \
 && mkdir -p /out/pg \
 && cp /src/megapolos-core/install/newpostgresql.sql /out/pg/newpostgresql.sql

COPY installer/megapolos-installer /out/installer
COPY install/bootstrap.sh /out/install.sh
RUN chmod +x /out/installer /out/install.sh

# ---------- 5. debs + INFO + squashfs ----------
RUN mkdir -p /out/debs \
 && find /var/cache/apt/archives -maxdepth 1 -name '*.deb' -exec cp {} /out/debs/ \; \
 && { echo "Megapolos offline bundle (docker build)"; \
      echo "Дата: $(date -Iseconds)"; \
      echo "Содержимое: debs=$(ls /out/debs | wc -l), npm-кэши, node-gyp, repos, pg-дамп, installer"; \
      echo; \
      echo "Использование:"; \
      echo "  sudo mount -o ro,loop megapolos-bundle.sqfs /mnt/megapolos-bundle"; \
      echo "  sudo /mnt/megapolos-bundle/installer"; \
    } > /out/INFO.txt \
 && mkdir -p /bundle \
 && mksquashfs /out /bundle/megapolos-bundle.sqfs -comp zstd -noappend

CMD ["ls", "-la", "/bundle"]
