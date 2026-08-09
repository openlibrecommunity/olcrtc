<div align="center">

<img src="https://github.com/openlibrecommunity/material/blob/master/olcrtc.png" width="250" height="250">

![License](https://img.shields.io/badge/license-WTFPL-0D1117?style=flat-square&logo=open-source-initiative&logoColor=green&labelColor=0D1117)
![Golang](https://img.shields.io/badge/-Golang-0D1117?style=flat-square&logo=go&logoColor=00A7D0)

**RU** / [EN](manual.md)

</div>

# Мануальная сборка

> **Важно:** Обязательно проверяйте, есть ли сервис видеозвонков у вас в белых списках. Если его там нет - используйте другой. Список всех сервисов в белых списках скоро будет опубликован.


Этот способ для тех кто хочет собрать нативный бинарник руками.
Нужен Go 1.26+, mage, git.

---


### swap (ОЗУ)

Если у вас меньше 4ГБ оперативной памяти, сборка может вылетать. **Обязательно включите SWAP**:

```bash
sudo fallocate -l 4G /swapfile && sudo chmod 600 /swapfile && sudo mkswap /swapfile && sudo swapon /swapfile
```


---

## Что нужно установить

## Шаг 1: Установить git

```sh
apt install git       # Debian   / Ubuntu  / Mint
pacman -S git         # Arch    / CachyOS / Manjaro
dnf install git       # Fedora / RHEL   / CentOS
```

---

## Шаг 2: Установить Go 1.26+

### Arch / Fedora (всё просто)

```sh
pacman -S go    # Arch    / CachyOS / Manjaro
dnf install go  # Fedora / RHEL   / CentOS
```

### Debian / Ubuntu (системный пакет устаревший)

На Debian/Ubuntu в репозитории обычно Go 1.19.

На Debian 13 лучше через `testing` c `APT Pinning`, чтобы не засорять ОС:

```sh
echo 'deb http://deb.debian.org/debian/ testing main non-free-firmware' | sudo tee /etc/apt/sources.list.d/testing.list

cat <<EOF | sudo tee /etc/apt/preferences.d/testing-pin
Package: *
Pin: release a=testing
Pin-Priority: 100
EOF

sudo apt update
sudo apt install -t testing golang-go

sudo update-alternatives --install /usr/bin/go go `which go` 10
sudo update-alternatives --install /usr/bin/gofmt gofmt `which gofmt` 10
```

Иначе через SDK:

```sh
apt install golang                         # ставим старый go - он нужен только чтобы скачать новый
go install golang.org/dl/go1.26.0@latest   # скачиваем установщик go1.26
~/go/bin/go1.26.0 download                 # скачиваем сам go1.26
mv ~/go/bin/go1.26.0 /usr/local/bin/go     # заменяем системный go
```

### Проверка

```sh
go version
# go version go1.26.x linux/amd64
```

---

## Шаг 3: Установить mage

mage - система сборки для Go-проектов, аналог make.

```sh
go install github.com/magefile/mage@latest
```

Добавь `~/go/bin` в PATH:

```sh
echo 'export PATH="$HOME/go/bin:$PATH"' >> ~/.bashrc
source ~/.bashrc
```

Проверка:

```sh
mage --version
# mage vx.x.x
```

---

## Шаг 4: Скачать репозиторий

```sh
git clone https://github.com/openlibrecommunity/olcrtc
cd olcrtc
```


---

## Шаг 5: Собрать

```sh
mage build   # текущая платформа
mage cross   # все платформы сразу (если собираешь для другой машины)
```

Результат в `build/`:

```
build/olcrtc-linux-amd64
```

---

## Шаг 6: Сгенерировать ключ шифрования

Делается один раз на сервере. Ключ должен совпадать на сервере и клиенте.

```sh
openssl rand -hex 32 
# d823fa01cb3e0609b67322f7cf984c4ee2e4ce2e294936fc24ef38c9e59f4799
```

Сохрани вывод - понадобится при запуске клиента.

---

## Шаг 7: Запустить сервер

На серверной машине (VPS и т.д.). Подбери нужную комбинацию auth provider + transport из матрицы в [settings.md](settings.ru.md).

### jitsi + datachannel (рекомендуется)

Самый простой способ: используй любой self-hosted или публичный Jitsi Meet инстанс. Регистрация не нужна, имя комнаты выдумывается на лету. Доступные инстансы перечислены в [`docs/examples/jitsi.instances.yaml`](./examples/jitsi.instances.yaml) - **обязательно проверь в браузере, какой из них работает в твоей сети**, и используй тот, который открывается. Также подойдёт любой другой (`meet.jit.si`, свой self-hosted и т.п.).

Создай YAML конфиг:

```yaml
# server.yaml
mode: srv
auth:
  provider: jitsi
room:
  # Инстансы: docs/examples/jitsi.instances.yaml
  id: "https://meet.example.org/myroom"
crypto:
  key: "d823fa01cb3e0609b67322f7cf984c4ee2e4ce2e294936fc24ef38c9e59f4799"
net:
  transport: datachannel
  dns: "8.8.8.8:53"
data: data
```

Запусти:

```sh
./build/olcrtc-linux-amd64 server.yaml
```

Сервер сам присоединится к комнате (в качестве участника без камеры/микрофона) и будет ждать, пока клиент тоже зайдёт. Без второго участника Jicofo не выдаёт session-initiate - это особенность Jitsi.

### wbstream + vp8channel (альтернатива)

Создай руму через сайт [wbstream](https://stream.wb.ru) и вставь её ID в `room.id`.

`wbstream + datachannel` **не работает** в обычном guest flow - WB Stream выдаёт токены с `canPublishData=false`, и DC не маршрутизирует данные. Для обычного использования выбирай `vp8channel`.

Создай YAML конфиг:

```yaml
# server.yaml
mode: srv
auth:
  provider: wbstream
room:
  id: "<room-id-со-stream.wb.ru>"
crypto:
  key: "d823fa01cb3e0609b67322f7cf984c4ee2e4ce2e294936fc24ef38c9e59f4799"
net:
  transport: vp8channel
  dns: "8.8.8.8:53"
data: data
```

Запусти:

```sh
./build/olcrtc-linux-amd64 server.yaml
```

Room ID нужно передать клиенту.

### Добавить отладку

Добавь `debug: true` в YAML конфиг - увидишь каждое соединение:

```
2026/05/03 08:05:23 Connecting link via direct/vp8channel/wbstream...
2026/05/03 08:05:25 wbstream publisher state: connected
2026/05/03 08:05:27 Link connected
2026/05/03 08:05:43 sid=3 connect icanhazip.com:443
2026/05/03 08:05:43 sid=3 connected icanhazip.com
```

---

## Шаг 8: Запустить клиент

На своей машине. `auth.provider`, `net.transport`, `room.id` и `crypto.key` должны совпадать с сервером.

> Хочешь готовый Android-клиент вместо YAML-конфига? Возьми [owenewans/owenclave](https://github.com/owenewans/owenclave) ([src.owenewans.org/owenrtc](https://src.owenewans.org/owenrtc)) - он читает URI `olcrtc://` и подписки напрямую. Ниже - запуск нативного бинарника `cnc` (только SOCKS5).

### jitsi + datachannel (рекомендуется)

```yaml
# client.yaml
mode: cnc
auth:
  provider: jitsi
room:
  # Инстансы: docs/examples/jitsi.instances.yaml
  id: "https://meet.example.org/myroom"
crypto:
  key: "<hex-key-такой-же-как-на-сервере>"
net:
  transport: datachannel
  dns: "8.8.8.8:53"
socks:
  host: "127.0.0.1"
  port: 8808
data: data
```

```sh
./build/olcrtc-linux-amd64 client.yaml
```

После запуска SOCKS5 будет слушать на `127.0.0.1:8808`. Используй любой клиент с поддержкой SOCKS5 (`curl --socks5 127.0.0.1:8808 ...`, браузер с переключателем прокси и т.п.).

### wbstream + vp8channel (альтернатива)

```yaml
# client.yaml
mode: cnc
auth:
  provider: wbstream
room:
  id: "<room-id>"
crypto:
  key: "<hex-key>"
net:
  transport: vp8channel
  dns: "8.8.8.8:53"
socks:
  host: "127.0.0.1"
  port: 8808
data: data
```

```sh
./build/olcrtc-linux-amd64 client.yaml
```

После старта в логах появится:

```
SOCKS5 server listening on 127.0.0.1:8808
```

Если нужно защитить прокси логином и паролем (например на машине с несколькими пользователями), добавь `socks.user` и `socks.pass` в конфиг:

```yaml
# client.yaml
mode: cnc
auth:
  provider: wbstream
room:
  id: "<room-id>"
crypto:
  key: "<hex-key>"
net:
  transport: vp8channel
  dns: "8.8.8.8:53"
socks:
  host: "127.0.0.1"
  port: 8808
  user: myuser
  pass: mypass
data: data
```

Без этих полей аутентификация отключена - поведение прежнее.

---

## Шаг 9: Проверить

```sh
curl --socks5-hostname 127.0.0.1:8808 https://icanhazip.com
```

Должен вернуть IP сервера.


---

## Обновить бинарник и уже запущенный инстанс

Запущенный процесс сам не обновляется: он продолжает работать со старым бинарником, даже если репозиторий уже обновлён. Нужно скачать свежий код, пересобрать бинарник и перезапустить процесс или systemd-сервис.

### 1. Обновить репозиторий

```sh
cd olcrtc
git pull --recurse-submodules
```

Если работаешь не с `master`, сначала переключись на нужную ветку:

```sh
git switch dev
git pull --recurse-submodules
```

### 2. Пересобрать бинарник

```sh
mage build
```

Для текущей Linux-машины результат обычно будет здесь:

```sh
build/olcrtc-linux-amd64
```

Для другой архитектуры смотри имя файла в `build/` или собирай все платформы сразу:

```sh
mage cross
ls build/
```

### 3. Остановить старый процесс

Если запускал вручную в терминале - останови его через `Ctrl+C`.

Если процесс запущен в фоне, найди его:

```sh
pgrep -af olcrtc
```

И останови нужный PID:

```sh
kill <pid>
```

Если olcrtc запущен через systemd, останавливать руками не нужно - достаточно рестарта сервиса после обновления бинарника.

### 4. Заменить бинарник, если он лежит вне `build/`

Если запускаешь прямо из `build/`, этот шаг не нужен.

Если копировал бинарник в системный путь, обнови копию:

```sh
sudo install -m 0755 build/olcrtc-linux-amd64 /usr/local/bin/olcrtc
```

Путь и имя файла могут отличаться, если машина не `linux/amd64`.

### 5. Запустить заново с тем же конфигом

Сервер:

```sh
./build/olcrtc-linux-amd64 server.yaml
```

Клиент:

```sh
./build/olcrtc-linux-amd64 client.yaml
```

Если используешь systemd:

```sh
sudo systemctl restart olcrtc-server
sudo systemctl restart olcrtc-client
```

Имена сервисов зависят от того, как ты их создавал. Конфиги менять не нужно, если `auth`, `transport`, `room ID`, ключ и SOCKS5-порт остаются прежними.

---

## Все mage таргеты

### Сборка
```sh
mage build    # собрать для текущей платформы
mage cross    # собрать для всех платформ
mage mobile   # собрать Android AAR
mage clean    # удалить build/
```

### Качество
```sh
mage vet      # go vet
mage lint     # golangci-lint
mage tidy     # go mod tidy && go mod verify
mage deps     # go mod download
```

### Тесты
```sh
mage test       # юниты в -short, быстро
mage testFull   # все юниты + локальные e2e с -race
mage e2e        # smoke-матрица против реальных провайдеров
mage stress     # stress-матрица (~6 ч)
mage soak       # реальный soak (часами)
mage localSoak  # in-memory soak (без сети)
```

### Пайплайны
```sh
mage check       # build + vet + lint + testFull (перед коммитом)
mage all         # check + e2e (перед мерджем PR)
mage nightly     # all + stress (ночной CI, ~6 ч)
mage everything  # nightly + soak + localSoak (полная валидация, 12+ ч)
```

### Прочее
```sh
mage help     # список таргетов в стандартном стиле mage
mage -l       # то же что mage help
mage         # без аргументов = mage help
```

Тонкая настройка прогона тестов через переменные окружения:

```sh
# одиночный кейс stress
E2E_CARRIERS=telemost E2E_TRANSPORTS=videochannel \
    STRESS_BULK_DURATION=0 STRESS_ECHO_DURATION=0 \
    STRESS_CASE_TIMEOUT=2m STRESS_TIMEOUT=3m mage stress

# soak только jitsi на 30 минут
SOAK_CARRIERS=jitsi SOAK_DURATION=30m mage soak
```

Полный список переменных:
- `E2E_CARRIERS`, `E2E_TRANSPORTS`, `E2E_TIMEOUT`, `E2E_STRESS`, `E2E_STRESS_DURATION`
- `STRESS_BULK_DURATION`, `STRESS_ECHO_DURATION`, `STRESS_CASE_TIMEOUT`, `STRESS_TIMEOUT`
- `SOAK_CARRIERS`, `SOAK_TRANSPORTS`, `SOAK_DURATION`, `SOAK_CHAOS`

---

## Несколько инстансов на одном сервере

Можно запустить несколько серверов olcrtc на одной машине - каждый со своим конфигом (разные провайдеры, комнаты, транспорты). Для этого создай отдельный YAML-файл для каждого инстанса и запусти каждый в отдельном процессе.

### Пример: два сервера

```yaml
# server-jitsi.yaml
mode: srv
auth:
  provider: jitsi
room:
  id: "https://meet.example.org/room1"
crypto:
  key: "aaaa...1111"
net:
  transport: datachannel
  dns: "8.8.8.8:53"
data: data
```

```yaml
# server-wbstream.yaml
mode: srv
auth:
  provider: wbstream
room:
  id: "<room-id>"
crypto:
  key: "bbbb...2222"
net:
  transport: vp8channel
  dns: "8.8.8.8:53"
data: data
```

Запусти каждый в отдельном терминале (или через `tmux` / `screen` / `systemd`):

```sh
./build/olcrtc-linux-amd64 server-jitsi.yaml
./build/olcrtc-linux-amd64 server-wbstream.yaml
```

### Клиенты

На клиентской машине - по одному конфигу на каждый сервер, с **разными SOCKS5 портами**:

```yaml
# client-jitsi.yaml
mode: cnc
auth:
  provider: jitsi
room:
  id: "https://meet.example.org/room1"
crypto:
  key: "aaaa...1111"
net:
  transport: datachannel
  dns: "8.8.8.8:53"
socks:
  host: "127.0.0.1"
  port: 8808
data: data
```

```yaml
# client-wbstream.yaml
mode: cnc
auth:
  provider: wbstream
room:
  id: "<room-id>"
crypto:
  key: "bbbb...2222"
net:
  transport: vp8channel
  dns: "8.8.8.8:53"
socks:
  host: "127.0.0.1"
  port: 8809
data: data
```

```sh
./build/olcrtc-linux-amd64 client-jitsi.yaml      # SOCKS5 на :8808
./build/olcrtc-linux-amd64 client-wbstream.yaml    # SOCKS5 на :8809
```

Переключение между инстансами в olcbox - просто выбираешь нужный SOCKS5 порт.

---

Нужен короткий путь без подробностей? -> [Быстрый старт](fast.ru.md)

Все настройки и матрица совместимости -> [settings.md](settings.ru.md)
