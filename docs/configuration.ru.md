<div align="center">

<img src="https://github.com/openlibrecommunity/material/blob/master/olcrtc.png" width="250" height="250">

![License](https://img.shields.io/badge/license-WTFPL-0D1117?style=flat-square&logo=open-source-initiative&logoColor=green&labelColor=0D1117)
![Golang](https://img.shields.io/badge/-Golang-0D1117?style=flat-square&logo=go&logoColor=00A7D0)

**RU** / [EN](configuration.md)

</div>


# Настройка YAML

`olcrtc` читает runtime-настройки из одного YAML-файла. CLI принимает ровно один аргумент - путь к конфигу; отдельных CLI-флагов для режима, транспорта и провайдера больше нет.

```bash
olcrtc /etc/olcrtc/server.yaml
olcrtc /etc/olcrtc/client.yaml
```

Готовые примеры:

- [`server.jitsi.datachannel.yaml`](./examples/server.jitsi.datachannel.yaml) - jitsi + datachannel srv
- [`client.jitsi.datachannel.yaml`](./examples/client.jitsi.datachannel.yaml) - jitsi + datachannel cnc
- [`server.jitsi.videochannel.yaml`](./examples/server.jitsi.videochannel.yaml) - jitsi + videochannel srv
- [`client.jitsi.videochannel.yaml`](./examples/client.jitsi.videochannel.yaml) - jitsi + videochannel cnc
- [`server.jitsi.seichannel.yaml`](./examples/server.jitsi.seichannel.yaml) - jitsi + seichannel srv
- [`client.jitsi.seichannel.yaml`](./examples/client.jitsi.seichannel.yaml) - jitsi + seichannel cnc
- [`server.jitsi.vp8channel.yaml`](./examples/server.jitsi.vp8channel.yaml) - jitsi + vp8channel srv
- [`client.jitsi.vp8channel.yaml`](./examples/client.jitsi.vp8channel.yaml) - jitsi + vp8channel cnc
- [`server.telemost.datachannel.yaml`](./examples/server.telemost.datachannel.yaml) - telemost + datachannel srv
- [`client.telemost.datachannel.yaml`](./examples/client.telemost.datachannel.yaml) - telemost + datachannel cnc
- [`server.telemost.videochannel.yaml`](./examples/server.telemost.videochannel.yaml) - telemost + videochannel srv
- [`client.telemost.videochannel.yaml`](./examples/client.telemost.videochannel.yaml) - telemost + videochannel cnc
- [`server.telemost.seichannel.yaml`](./examples/server.telemost.seichannel.yaml) - telemost + seichannel srv
- [`client.telemost.seichannel.yaml`](./examples/client.telemost.seichannel.yaml) - telemost + seichannel
- [`server.telemost.vp8channel.yaml`](./examples/server.telemost.vp8channel.yaml) - telemost + vp8channel srv
- [`client.telemost.vp8channel.yaml`](./examples/client.telemost.vp8channel.yaml) - telemost + vp8channel cnc
- [`server.wbstream.datachannel.yaml`](./examples/server.wbstream.datachannel.yaml) - wbstream + datachannel srv
- [`client.wbstream.datachannel.yaml`](./examples/client.wbstream.datachannel.yaml) - wbstream + datachannel cnc
- [`server.wbstream.videochannel.yaml`](./examples/server.wbstream.videochannel.yaml) - wbstream + videochannel srv
- [`client.wbstream.videochannel.yaml`](./examples/client.wbstream.videochannel.yaml) - wbstream + videochannel cnc
- [`server.wbstream.seichannel.yaml`](./examples/server.wbstream.seichannel.yaml) - wbstream + seichannel srv
- [`client.wbstream.seichannel.yaml`](./examples/client.wbstream.seichannel.yaml) - wbstream + seichannel cnc
- [`server.wbstream.vp8channel.yaml`](./examples/server.wbstream.vp8channel.yaml) - wbstream + vp8channel srv
- [`client.wbstream.vp8channel.yaml`](./examples/client.wbstream.vp8channel.yaml) - wbstream + vp8channel cnc
- [`failover.yaml`](./examples/failover.yaml) - failover

## Схема

| YAML path | Значение |
|---|---|
| `mode` | `srv`, `cnc` или `gen` |
| `auth.provider` | `jitsi`, `telemost`, `wbstream`, `none` |
| `room.id` | ID/URL комнаты для выбранного auth-провайдера |
| `room.channel` | необязательный ID канала для peer-routing сценариев |
| `crypto.key` / `crypto.key_file` | общий ключ: 64 hex-символа, напрямую или из файла |
| `net.transport` | `datachannel`, `vp8channel`, `seichannel`, `videochannel` |
| `net.dns` | DNS resolver в формате `host:port` |
| `socks.host` / `socks.port` | локальный SOCKS5 listener в `mode: cnc` |
| `socks.user` / `socks.pass` | необязательная auth для входящих SOCKS5-подключений |
| `socks.proxy_addr` / `socks.proxy_port` | исходящий SOCKS5-прокси на серверной стороне |
| `socks.proxy_user` / `socks.proxy_pass` | необязательная auth для upstream-прокси (RFC 1929) |
| `engine.name` / `engine.url` / `engine.token` | прямой engine-режим, только при `auth.provider: none` |
| `video.*` | настройки `videochannel` |
| `vp8.*` | настройки `vp8channel` |
| `sei.*` | настройки `seichannel` |
| `liveness.interval` | интервал ping по control stream, по умолчанию `10s` |
| `liveness.timeout` | таймаут pong, по умолчанию `5s` |
| `liveness.failures` | сколько pong можно пропустить до rebuild, по умолчанию `3` |
| `lifecycle.max_session_duration` | плановый rebuild сессии, например `6h`; пусто = выключено |
| `traffic.max_payload_size` | лимит зашифрованного wire-message; `0` = лимит транспорта |
| `traffic.min_delay` / `traffic.max_delay` | необязательный pacing отправки, например `5ms` / `30ms` |
| `gen.amount` | режим `gen`: сколько комнат создать |
| `profiles[]` | список failover-профилей для `srv`/`cnc` |
| `failover.retry_delay` | пауза перед следующим профилем, например `2s` |
| `failover.max_cycles` | сколько полных проходов по профилям сделать; `0` = бесконечно |
| `data` | путь к директории с runtime-данными (`names`, `surnames`) |
| `debug` | подробное логирование |
| `ffmpeg` | путь к бинарнику ffmpeg для `videochannel` |

`crypto.key_file` читается относительно YAML-файла. Нельзя одновременно задавать `crypto.key` и `crypto.key_file`.

`mode: cnc` запрещает слушать не-loopback адрес (`0.0.0.0`, LAN IP и т.п.), если не заданы оба поля `socks.user` и `socks.pass`.

## Обязательный минимум

### Сервер

> **Jitsi-провайдер:** используйте тот сервер, который доступен в вашей сети. Проверьте в браузере и выберите рабочий:
> - `https://meet.small-dm.ru/`
> - `https://meet1.arbitr.ru/` 
> - `https://meet.handyweb.org/`

```yaml
mode: srv
auth:
  provider: jitsi
room:
  # Используйте тот Jitsi-сервер, который работает в вашей сети:
  # https://meet.small-dm.ru/ROOM  или  https://meet1.arbitr.ru/ROOM  или  https://meet.handyweb.org/ROOM
  id: "https://meet.small-dm.ru/REPLACE_ME_WITH_ROOM_ID"
crypto:
  key: "REPLACE_ME_WITH_64_HEX_CHARS"
net:
  transport: datachannel
  dns: "8.8.8.8:53"
data: data
```

### Клиент

```yaml
mode: cnc
auth:
  provider: jitsi
room:
  # Используйте тот Jitsi-сервер, который работает в вашей сети:
  # https://meet.small-dm.ru/ROOM  или  https://meet1.arbitr.ru/ROOM  или  https://meet.handyweb.org/ROOM
  id: "https://meet.small-dm.ru/REPLACE_ME_WITH_ROOM_ID"
crypto:
  key: "REPLACE_ME_WITH_64_HEX_CHARS"
net:
  transport: datachannel
  dns: "8.8.8.8:53"
socks:
  host: "127.0.0.1"
  port: 8808
data: data
```

## Liveness

После `CLIENT_HELLO` / `SERVER_WELCOME` первый smux stream остаётся открытым как зашифрованный control stream. По нему `olcrtc` отправляет `CONTROL_PING` / `CONTROL_PONG`, чтобы проверять именно рабочий путь туннеля, а не только статус WebRTC-соединения.

```yaml
liveness:
  interval: 10s
  timeout: 5s
  failures: 3
```

Когда порог пропущенных pong достигнут, текущая smux-сессия пересоздаётся. В failover-режиме профиль, который завершился после неудачного reconnect, отдаёт управление supervisor, и тот пробует следующий профиль.

## Lifecycle Rotation

`lifecycle.max_session_duration` задаёт плановый верхний предел длительности одного звонка/сессии у провайдера. Когда время истекает, активная `srv` или `cnc` сессия закрывается и запускается заново с тем же конфигом.

```yaml
lifecycle:
  max_session_duration: 6h
```

Поле необязательное. Формат - Go duration: `30m`, `2h`, `6h`. Ноль и отрицательные значения не принимаются.

## Traffic Shaping

`traffic` добавляет общий wrapper вокруг выбранного транспорта. Он может ограничить размер зашифрованного сообщения и добавить небольшую задержку перед отправкой. Данные не обрезаются: если payload не помещается в эффективный лимит, отправка завершается явной ошибкой.

```yaml
traffic:
  max_payload_size: 4096
  min_delay: 5ms
  max_delay: 30ms
```

Лимит сжимается до `MaxPayloadSize`, который заявляет выбранный транспорт. Клиент и сервер также уменьшают smux frame size с учётом crypto overhead. Значение `0` не добавляет лимит сверх лимита транспорта. Если задан только `min_delay`, задержка фиксированная. Используй одинаковые `traffic`-настройки на обеих сторонах.

## Failover Profiles

`mode: srv` и `mode: cnc` могут задавать `profiles`. Верхнеуровневые поля становятся общими defaults, а каждый профиль переопределяет только то, что указано внутри него.

```yaml
mode: srv
crypto:
  key_file: ./olcrtc.key
net:
  dns: "8.8.8.8:53"
data: data

profiles:
  - name: wb-vp8
    auth:
      provider: wbstream
    room:
      id: "WB_ROOM_ID"
    net:
      transport: vp8channel

  - name: jitsi-dc
    auth:
      provider: jitsi
    room:
      id: "https://meet.example.org/olcrtc-room"
    net:
      transport: datachannel

failover:
  retry_delay: 2s
  max_cycles: 0
```

Порядок профилей и параметры комнаты должны быть совместимы на сервере и клиенте. Активные smux streams между профилями не мигрируют; новые подключения смогут восстановиться на следующем профиле.

## mode: gen

`gen` оставлен для auth-провайдеров, которые реализуют создание комнат через API.
Текущие встроенные провайдеры (`jitsi`, `telemost`, `wbstream`) не создают комнаты
через `olcrtc`: для `telemost` и `wbstream` создай комнату на сайте сервиса и
вставь её в `room.id`; для `jitsi` укажи URL комнаты.
