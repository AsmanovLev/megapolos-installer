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

## Почему установщик вызывает `install.ts`, а не портит его в Go

Возникал вопрос: «почему бы всё не перевести в монолит на Go — один статический
бинарник быстрее». Аргументы против порта (решенo: вызываем `npm run bootstrap`):

1. **Время install.ts — это не язык.** Старт ts-node ~2 сек; остальные минуты —
   ansible-прогоны и docker build (I/O и сеть). Go их не ускорит. А Node.js в
   системе всё равно обязателен: ядро — Node-приложение.
2. **install.ts — это не скрипт, а внутренности платформы.** Он дергает
   NodeRepo/AppRepo/ImageRepo поверх Mikro-ORM, рендерит ansible-шаблоны,
   собирает образа, раздаёт CA. Порт = переписать кусок megapolos-core
   (тысячи строк) и потом **вечно догонять upstream** (файл правится каждые
   несколько недель; семантическое расхождение вылезет в рантайме на нодах).
3. **Контракт вызова узкий и стабильный**: env `MEGAPOLOS_NODE_*`,
   `MEGAPOLOS_BOOTSTRAP_APP_*`. Его и держим. Порт логики — широкий и живой.
4. Установщик остаётся монолитом там, где это даёт выигрыш: детект пакетов,
   оффлайн-бандл, идемпотентность, TUI — это наш код и наш контроль.

Когда монолит стал бы оправдан: если платформа опубликует стабильный API
bootstrap'а (GraphQL-мутации init/prepare/installRegistry покрывают всё) ИЛИ
если upstream бросит install.ts. Тогда — GraphQL-оркестрация из Go без ts-node.

