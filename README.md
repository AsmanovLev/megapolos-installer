# megapolos-installer

Установщик Megapolos (megapolos-core + megapolos-gui) — один статический Go-бинарь
с TUI (tview) и headless-режимом. Работает онлайн (gitlab/кэши хоста) и полностью
оффлайн (squashfs-бандл).

## Быстрый старт

Онлайн (внутри целевой VM/машины):

```bash
curl -fsSL http://<host>:8000/install/bootstrap.sh | sudo bash   # скачает бандл, смонтирует, запустит
```

Из смонтированного бандла:

```bash
sudo mount -o ro,loop megapolos-bundle.sqfs /mnt/megapolos-bundle
sudo /mnt/megapolos-bundle/installer            # TUI
sudo /mnt/megapolos-bundle/installer --no-tui --yes   # headless
```

## Режимы

- TUI-визард (по умолчанию при наличии терминала): источник репо, ветки/коммиты,
  API URL, devMode, базовый домен, GUI on/off, self-node.
- Headless: всё через флаги (`megapolos-installer --help`), env `MEGAPOLOS_*`.
- Идемпотентен: повторный прогон пропускает сделанное (с причинами skip).

## Сборка

Go не нужен на хосте — сборка в контейнере (podman, fallback docker):

```bash
podman run --rm -v .:/src:Z -v ./vm/cache/gomod:/go/pkg/mod:Z -w /src \
  golang:1.24-bookworm bash -c 'CGO_ENABLED=0 go build -o megapolos-installer .'
```

Тесты: добавь `go vet ./... && go test ./...` перед build.

## Оффлайн-бандл (squashfs)

```bash
./vm/make-bundle.sh 2222          # из установленной VM (ssh-порт)
./vm/make-bundle-docker.sh        # из Dockerfile, без VM-донора
```

Получается `bundle/megapolos-bundle.sqfs` (~650M): debs, npm-кэши, git-репо,
дамп БД, node-gyp кэш, lock-файлы, сам бинарь.

## Тестовые VM (QEMU/KVM)

```bash
./vm/host-services.sh start       # http :8000 (зеркало), apt-cacher :3142, verdaccio :4873
./vm/create-vm.sh demo            # user-net (пробросы портов)
./vm/create-vm.sh lan --net bridge  # bridge в LAN (свой IP из DHCP)
./vm/vm.sh demo start|stop|ssh|reset|status|log
./vm/e2e.sh test2 --offline       # E2E: reset → install → ассерты
```

## Структура

- `main.go`, `internal/` — Go-установщик (steps DAG-runner, tview TUI, sys exec)
- `vm/` — инфраструктура тестовых VM и хост-сервисов
- `bundle/` — Dockerfile и скрипты сборки бандла (артефакты в .gitignore)
- `install/` — bootstrap.sh (однострочник) + legacy bash-установщик (справочно)

Исходники core/gui ожидаются в соседнем каталоге `../megapolos/{megapolos-core,megapolos-gui}`
(нужны host-services для git-зеркала и make-bundle для бандла).
