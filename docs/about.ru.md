<div align="center">

<img src="https://github.com/openlibrecommunity/material/blob/master/olcrtc.png" width="250" height="250">

![License](https://img.shields.io/badge/license-WTFPL-0D1117?style=flat-square&logo=open-source-initiative&logoColor=green&labelColor=0D1117)
![Golang](https://img.shields.io/badge/-Golang-0D1117?style=flat-square&logo=go&logoColor=00A7D0)

**RU** / [EN](about.md)

</div>



# olcRTC - общее описание

`olcRTC` (OpenLibreCommunity RTC) - зашифрованный TCP-over-WebRTC туннель. Он маскирует трафик под обычное участие в WebRTC/SFU-сервисе: Jitsi Meet, Yandex Telemost или WbStream.

Проект: [github.com/openlibrecommunity/olcrtc](https://github.com/openlibrecommunity/olcrtc)  
Лицензия: WTFPL  
Статус: **Beta**

## Зачем это нужно

В сценариях, где прямой доступ к произвольному VPS / IP заблокирован, приходится переносить трафик через сервисы, которые уже доступны у пользователя. Для внешнего наблюдателя соединение выглядит как обычный WebRTC-звонок по разрешенному IP сервиса, а полезная нагрузка внутри дополнительно шифруется общим ключом `crypto.key`.

> **Важно:** Обязательно проверяйте, есть ли сервис видеозвонков у вас в белых списках. Если его там нет - используйте другой. Список всех сервисов в белых списках скоро будет опубликован.

Базовая схема:

```text
приложение
  -> SOCKS5 127.0.0.1:8808
   -> olcrtc cnc
    -> WebRTC/SFU сервис
     -> olcrtc srv
       -> интернет
```

## Как это работает

Клиентский режим `cnc` поднимает локальный SOCKS5. Браузер, curl, sing-box, olcbox или другое приложение подключается к нему как к обычному proxy.

Серверный режим `srv` подключается к той же комнате/сессии, принимает зашифрованный smux stream и от своего имени открывает TCP-соединения к целевым адресам.

Внутри туннеля:

```text
SOCKS CONNECT
  -> smux stream
   -> XChaCha20-Poly1305
    -> transport
     -> engine
      -> WebRTC/SFU
```

## Режимы

| Режим | Назначение |
|---|---|
| `srv` | серверная сторона, принимает tunnel streams и делает TCP dial к целям |
| `cnc` | клиентская сторона, слушает локальный SOCKS5 |
| `gen` | создаёт Room ID для провайдеров, которые умеют создавать комнаты |

CLI принимает один YAML-файл:

```bash
olcrtc server.yaml
olcrtc client.yaml
```

## Auth Providers

`auth.provider` выбирает сервис и способ получения credentials.

| Provider | Engine | Комментарий |
|---|---|---|
| `jitsi` | `jitsi` | URL комнаты Jitsi, инстансы в docs/examples/jitsi.instances.yaml, без отдельной регистрации |
| `telemost` | `goolom` | credentials через Yandex Telemost API, с отдельной регистрацией |
| `wbstream` | `livekit` | credentials через WbBStream API, с отдельной регистрацией |
| `none` | задаётся в `engine.name` | прямой engine-режим с `engine.url` и `engine.token`, с отдельной регистрацией |

Термин `carrier` ещё встречается во внутреннем API и логах как историческое имя для выбранного auth/provider пути. В YAML актуальное поле - `auth.provider`.

## Engines

`engine` - низкоуровневый протокол конкретного SFU/signaling:

| Engine | Пакет | Возможности |
|---|---|---|
| `livekit` | `internal/engine/livekit` | data packets/video tracks/LiveKit SDK |
| `goolom` | `internal/engine/goolom` | Telemost/Goolom signaling, publisher/subscriber PeerConnection |
| `jitsi` | `internal/engine/jitsi` | Jitsi MUC/Jingle/colibri-ws, datachannel/best-effort video |

`internal/engine/builtin` связывает `auth.provider` с нужным engine. Отдельного пакета `internal/carrier` в текущем проекте нет.

## Transports

`net.transport` определяет, как tunnel bytes помещаются в WebRTC primitive.

| Transport | Как передаёт данные | Основной сценарий |
|---|---|---|
| `datachannel` | нативный byte/data path engine | самый простой и быстрый путь, стабильно с Jitsi |
| `vp8channel` | KCP поверх VP8-like video frames | основной video-path для WB Stream и Telemost |
| `seichannel` | payload в H264 SEI NAL units, ACK/retry | fallback для WB Stream / Jitsi|
| `videochannel` | QR/tile кадры через ffmpeg, ACK/retry | экспериментальный визуальный транспорт |

Рекомендуемый старт: `jitsi + datachannel`. Альтернатива: `wbstream + vp8channel`.

## Шифрование и handshake

`internal/crypto` использует XChaCha20-Poly1305. Общий ключ задаётся как 64 hex-символа:

```bash
openssl rand -hex 32
```

Поверх зашифрованного `muxconn` запускается `smux`. Первый smux stream занят handshake и control protocol:

```text
CLIENT_HELLO -> SERVER_WELCOME
CONTROL_PING <-> CONTROL_PONG
```

Если control pong не приходит несколько раз подряд, runtime пересобирает smux-сессию или отдаёт управление failover supervisor.

## YAML

Минимальный сервер:

```yaml
mode: srv
auth:
  provider: jitsi
room:
  # Используйте тот Jitsi-сервер, который работает в вашей сети:
  # Инстансы: docs/examples/jitsi.instances.yaml - https://HOST/ROOM
  id: "https://meet.example.org/REPLACE_ME_WITH_ROOM_ID"
crypto:
  key: "REPLACE_ME_WITH_64_HEX_CHARS"
net:
  transport: datachannel
  dns: "8.8.8.8:53"
data: data
```

Минимальный клиент:

```yaml
mode: cnc
auth:
  provider: jitsi
room:
  # Используйте тот Jitsi-сервер, который работает в вашей сети:
  # Инстансы: docs/examples/jitsi.instances.yaml - https://HOST/ROOM
  id: "https://meet.example.org/REPLACE_ME_WITH_ROOM_ID"
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

Подробнее: [configuration.md](configuration.ru.md), [settings.md](settings.ru.md).

## Failover

`profiles[]` позволяет запускать несколько конфигураций по порядку. Например, сначала `wbstream + vp8channel`, потом `jitsi + datachannel`. Верхнеуровневые поля работают как defaults, профиль переопределяет только нужные части.

Активные smux streams при смене профиля не мигрируют. Новые подключения смогут подняться на следующем профиле.

## Структура репозитория

| Путь | Что внутри |
|---|---|
| `cmd/olcrtc` | CLI entrypoint |
| `cmd/olcrtc-cgo` | c-shared entrypoint |
| `pkg/olcrtc` | embeddable client/engine API |
| `pkg/olcrtc/tunnel` | embeddable server tunnel API |
| `mobile` | gomobile bindings для Android |
| `internal/config` | YAML parsing, `crypto.key_file` |
| `internal/app/session` | defaults, validation, routing в `srv`/`cnc`/`gen` |
| `internal/auth` | provider-specific credential flows |
| `internal/engine` | SFU/signaling implementations |
| `internal/transport` | datachannel/vp8/sei/video transports |
| `internal/server` | server-side smux, handshake, TCP dial |
| `internal/client` | SOCKS5 listener, client-side smux |
| `internal/control` | liveness ping/pong |
| `internal/supervisor` | failover profiles |
| `docs` | документация и примеры YAML |

## Сборка

```bash
go install github.com/magefile/mage@latest

mage build
mage cross
mage test
mage lint
mage mobile
```

Go версия: `1.26+`. Для `videochannel` нужен `ffmpeg`; для `codec: tile` требуется разрешение `1080x1080`.

## Public API

`pkg/olcrtc` возвращает `net.Conn`-подобный объект поверх auth/engine:

```go
sess, err := olcrtc.New(ctx, olcrtc.Config{
    Auth:   "jitsi",
    // Инстансы: docs/examples/jitsi.instances.yaml
    RoomID: "https://meet.example.org/myroom",
})
if err != nil {
    return err
}
conn, err := sess.Dial(ctx)
```

`pkg/olcrtc/tunnel` встраивает серверную сторону и даёт hooks:

```go
srv := tunnel.New(tunnel.Config{
    Transport: "datachannel",
    Carrier:   "jitsi",
    // Инстансы: docs/examples/jitsi.instances.yaml
    RoomURL:   "https://meet.example.org/myroom",
    KeyHex:    "<64-char hex>",
    DNSServer: "8.8.8.8:53",
})
err := srv.Run(ctx)
```

В этом API поле `Carrier` сохранено ради совместимости с существующими интеграциями; по смыслу это имя `auth.provider`.

## Mobile / Android

`mobile/mobile.go` предоставляет gomobile API:

- `SetProtector` для Android VPN `protect(fd)`;
- `SetTransport`, `SetDNS`, `SetVP8Options`, `SetLivenessOptions`;
- `Start`, `StartWithTransport`, `Stop`;
- `Check`/ping helpers для проверки доступности.

По умолчанию mobile-клиент использует `vp8channel`; `datachannel` тоже поддерживается.

## Клиенты

Готовые клиенты, которые говорят на `olcrtc`:

| Клиент | Роль | Протоколы |
|---|---|---|
| [owenewans/owenclave](https://github.com/owenewans/owenclave) ([src.owenewans.org/owenrtc](https://src.owenewans.org/owenrtc)) | **основной клиент**, Android (форк exclave) | все распространённые протоколы (vless, hysteria2, mieru, trojan, vmess, tuic, shadowsocks, socks ...) плюс `olcrtc`, формат URI `olcrtc://` и подписки |
| [venterum/veil](https://github.com/venterum/veil) | клиент сообщества, Android (форк v2rayNG), Material 3 | VMess, VLESS, Shadowsocks, Trojan, SOCKS, WireGuard, Hysteria2 + `olcrtc` |
| [alananisimov/olcbox](https://github.com/alananisimov/olcbox) | клиент сообщества, мультиплатформенный (Android, iOS, macOS, Windows, Linux) | Все провайдеры (Jitsi, Telemost, WB Stream, Jazz), все транспорты, split tunneling, режимы TUN/proxy |

`owenclave` - референсный клиент для URI `olcrtc://` и формата подписки. Нативный бинарник `olcrtc` в `mode: cnc` - тоже полноценный клиент, он только поднимает SOCKS5-слушатель без UI.

## Тесты

```bash
go test -count=1 ./...
mage test
mage e2e
```

Real-provider E2E включаются через переменные:

```bash
E2E_CARRIERS=wbstream E2E_TRANSPORTS= vp8channel mage e2e
```

## Частые проблемы

| Симптом | Что проверить |
|---|---|
| `key required` или `invalid key` | на обеих сторонах одинаковый 64-символьный hex key |
| SOCKS5 не слушает | `mode: cnc`, `socks.host`, `socks.port`, логи клиента |
| Jitsi не соединяется без второго участника | сервер и клиент должны быть в одной комнате |
| WB Stream + datachannel не работает | в guest flow нет `canPublishData`; используй `vp8channel`, `seichannel` или `videochannel` |
| `seichannel ack timeout` | провайдер режет/не маршрутизирует video path; смени transport/provider |
| `ffmpeg` not found | установи ffmpeg или задай `ffmpeg: /path/to/ffmpeg` |

## Ссылки

- [Быстрый старт](fast.ru.md)
- [Ручная сборка](manual.ru.md)
- [Настройка YAML](configuration.ru.md)
- [Матрица совместимости](settings.ru.md)
- [URI формат](uri.ru.md)
- [Формат подписки](sub.ru.md)
