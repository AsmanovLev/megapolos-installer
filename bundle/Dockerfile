# =============================================================================
# megapolos-bundle — воспроизводимая сборка оффлайн-бандла в контейнере.
#
# Контекст сборки готовит vm/make-bundle-docker.sh (vm/run/bundle-ctx/):
#   megapolos-core/, megapolos-gui/  — клоны репозиториев
#   docker/                          — docker-образы (OS-независимы, tar.gz)
#   installer, install.sh            — бинарь установщика и bootstrap
#
#   ./vm/make-bundle-docker.sh [24.04|22.04]
# =============================================================================
ARG UBUNTU=24.04
FROM ubuntu:${UBUNTU}

ENV DEBIAN_FRONTEND=noninteractive
# официальный образ чистит /var/cache/apt/archives — нам debs нужны!
RUN rm -f /etc/apt/apt.conf.d/docker-clean \
 && printf '#!/bin/sh\nexit 101\n' > /usr/sbin/policy-rc.d && chmod +x /usr/sbin/policy-rc.d

# ---------- 1. базовые пакеты (без nodejs — он ниже, per-distro) ----------
RUN apt-get update && apt-get install -y --no-install-recommends \
      curl ca-certificates gnupg lsb-release git build-essential python3 \
      nginx openssl whiptail squashfs-tools docker.io docker-compose-v2 ansible \
      python3-docker python3-passlib python3-cryptography python3-jsondiff \
      python3-venv python3-pip

# ---------- 2. node 18 ----------
# noble: distro nodejs+npm (18.19); jammy: distro даёт node 12 → NodeSource 18
RUN . /etc/os-release; if [ "$VERSION_CODENAME" = "jammy" ]; then \
      curl -fsSL https://deb.nodesource.com/setup_18.x | bash - \
      && apt-get install -y nodejs; \
    else \
      apt-get install -y nodejs npm; \
    fi

# ---------- 3. PGDG + postgresql-16 (коденейм дистрибутива сам подставится) ----------
RUN install -d /usr/share/keyrings \
 && curl -fsSL https://www.postgresql.org/media/keys/ACCC4CF8.asc | gpg --dearmor -o /usr/share/keyrings/pgdg.gpg \
 && echo "deb [signed-by=/usr/share/keyrings/pgdg.gpg] http://apt.postgresql.org/pub/repos/apt $(lsb_release -cs)-pgdg main" \
      > /etc/apt/sources.list.d/pgdg.list \
 && apt-get update && apt-get install -y postgresql-16

# npm: устойчивость к флакающей сети (как в установщике)
RUN npm config set fetch-retries 5 \
 && npm config set fetch-retry-mintimeout 10000 \
 && npm config set fetch-retry-maxtimeout 120000 \
 && npm config set fetch-timeout 600000

# ---------- 4. исходники: node_modules + gui build + locks + node-gyp ----------
COPY megapolos-core /src/megapolos-core
COPY megapolos-gui  /src/megapolos-gui

RUN mkdir -p /out \
 && cd /src/megapolos-core && npm install \
 && tar czf /out/node_modules-megapolos-core.tar.gz -C /src/megapolos-core node_modules \
 && cd /src/megapolos-gui && npm install --force \
 && DISABLE_ESLINT_PLUGIN=true NODE_OPTIONS=--max-old-space-size=3072 npm run build \
 && tar czf /out/node_modules-megapolos-gui.tar.gz -C /src/megapolos-gui node_modules \
 && tar czf /out/gui-build.tar.gz -C /src/megapolos-gui build \
 && cp /src/megapolos-core/package-lock.json /out/package-lock.core.json \
 && cp /src/megapolos-gui/package-lock.json  /out/package-lock.gui.json \
 && (tar czf /out/node-gyp-cache.tar.gz -C /root .cache/node-gyp 2>/dev/null || true)

# ---------- 5. репозитории (file:// клоны), дамп БД, установщик ----------
# .git остаётся → git clone из бандла работает; node_modules выкидываем
# (они упакованы отдельными тарболами на шаге 4)
RUN mkdir -p /out/repos \
 && cp -a /src/megapolos-core /out/repos/megapolos-core.git \
 && cp -a /src/megapolos-gui  /out/repos/megapolos-gui.git \
 && rm -rf /out/repos/megapolos-core.git/node_modules \
          /out/repos/megapolos-gui.git/node_modules \
          /out/repos/megapolos-core.git/build \
          /out/repos/megapolos-gui.git/build \
 && mkdir -p /out/pg \
 && cp /src/megapolos-core/install/newpostgresql.sql /out/pg/newpostgresql.sql

COPY installer /out/installer
COPY pip /out/pip
COPY ansible-collections /out/ansible-collections
COPY install.sh /out/install.sh
RUN chmod +x /out/installer /out/install.sh

# ---------- 6. docker-образы (OS-независимы): registry/nginx/node/busybox ----------
COPY docker /out/docker

# ---------- 7. debs + INFO + squashfs ----------
ARG UBUNTU
RUN mkdir -p /out/debs \
 && find /var/cache/apt/archives -maxdepth 1 -name '*.deb' -exec cp {} /out/debs/ \; \
 && { echo "Megapolos offline bundle (docker build, ubuntu $UBUNTU)"; \
      echo "Дата: $(date -Iseconds)"; \
      echo "Содержимое: debs=$(ls /out/debs | wc -l), node_modules, gui-build, node-gyp, repos, pg-дамп, docker-образы, installer"; \
      echo; \
      echo "Использование:"; \
      echo "  sudo mount -o ro,loop megapolos-bundle-$UBUNTU.sqfs /mnt/megapolos-bundle"; \
      echo "  sudo /mnt/megapolos-bundle/installer"; \
    } > /out/INFO.txt \
 && mkdir -p /bundle \
 && mksquashfs /out /bundle/megapolos-bundle-$UBUNTU.sqfs -comp zstd -noappend

CMD ["ls", "-la", "/bundle"]
