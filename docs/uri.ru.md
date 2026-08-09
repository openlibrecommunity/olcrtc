<div align="center">

<img src="https://github.com/openlibrecommunity/material/blob/master/olcrtc.png" width="250" height="250">

![License](https://img.shields.io/badge/license-WTFPL-0D1117?style=flat-square&logo=open-source-initiative&logoColor=green&labelColor=0D1117)
![Golang](https://img.shields.io/badge/-Golang-0D1117?style=flat-square&logo=go&logoColor=00A7D0)

**RU** / [EN](uri.md)

</div>


# Краткий URI-формат для клиентов

Этот документ описывает **соглашение для разработчиков клиентских приложений**, которым нужен компактный способ передавать параметры подключения `olcrtc`.

Текущий `olcrtc` не парсит такой URI автоматически. Если клиентское приложение хочет использовать эту запись, оно должно само разобрать строку и передать полученные поля в YAML конфиг `olcrtc`.

Основной клиент, который потребляет этот формат URI - [owenewans/owenclave](https://github.com/owenewans/owenclave) ([src.owenewans.org/owenrtc](https://src.owenewans.org/owenrtc)) - Android-клиент прокси (форк exclave), поддерживающий все распространённые протоколы (vless, hysteria2, mieru, trojan, vmess, tuic, shadowsocks, socks ...) плюс `olcrtc` и подписки.

---

## Формат

```text
olcrtc://<Auth>?<Transport>@<RoomID>#<EncryptionKey>$<MIMO>
olcrtc://<Auth>?<Transport><key=value&key=value>@<RoomID>#<EncryptionKey>$<MIMO>
```

Все поля после `olcrtc://` считаются частью клиентского соглашения.

Блок `<key=value&...>` - payload параметров транспорта в угловых скобках, идёт сразу после имени транспорта. Если параметры транспорту не нужны или используются defaults - блок опускается целиком.

---

## Поля

| Поле | Значение |
|------|----------|
| `<Auth>` | Имя auth-провайдера, например `telemost`, `wbstream`, `jitsi` |
| `<Transport>` | Имя транспорта, например `datachannel`, `vp8channel`, `seichannel`, `videochannel` |
| payload | Параметры транспорта в `<key=value&...>`. Ключи совпадают с YAML полями. Блок опускается если используются defaults |
| `<RoomID>` | Идентификатор комнаты или auth-specific room URL/ID |
| `<EncryptionKey>` | Ключ шифрования в hex, обычно 64 символа (`32` байта) |
| `<MIMO>` | Свободный комментарий для UI/метаданных, например `RU / olc free sub / IPv6` |

---

## Параметры payload по транспортам

### datachannel

Payload не используется.

### vp8channel

| Ключ | YAML поле | Описание |
|------|-----------|----------|
| `vp8-fps` | `vp8.fps` | FPS VP8 потока |
| `vp8-batch` | `vp8.batch_size` | Кадров за тик |

### seichannel

| Ключ | YAML поле | Описание |
|------|-----------|----------|
| `fps` | `sei.fps` | FPS H264 потока |
| `batch` | `sei.batch_size` | Кадров за тик |
| `frag` | `sei.fragment_size` | Размер фрагмента в байтах |
| `ack-ms` | `sei.ack_timeout_ms` | Таймаут ACK в миллисекундах |

### videochannel

| Ключ | YAML поле | Описание |
|------|-----------|----------|
| `video-w` | `video.width` | Ширина в пикселях |
| `video-h` | `video.height` | Высота в пикселях |
| `video-fps` | `video.fps` | FPS |
| `video-bitrate` | `video.bitrate` | Битрейт, например `5000k` или `2M` |
| `video-hw` | `video.hw` | Аппаратное ускорение: `none` или `nvenc` |
| `video-codec` | `video.codec` | `qrcode` или `tile` |
| `video-qr-size` | `video.qr_size` | Размер фрагмента QR в байтах |
| `video-qr-recovery` | `video.qr_recovery` | Коррекция ошибок: `low` / `medium` / `high` / `highest` |
| `video-tile-module` | `video.tile_module` | Размер тайла в пикселях 1..270 (только `tile`) |
| `video-tile-rs` | `video.tile_rs` | Reed-Solomon паритет % 0..200 (только `tile`) |

---

## Соответствие YAML полям olcrtc

| URI поле | YAML поле |
|----------|-----------|
| `<Auth>` | `auth.provider` |
| `<Transport>` | `net.transport` |
| payload | соответствующие YAML поля транспорта |
| `<RoomID>` | `room.id` |
| `<EncryptionKey>` | `crypto.key` |
| `<MIMO>` | В `olcrtc` не передаётся. Это только клиентский комментарий |

`data: data` в этом формате не кодируется, потому что это локальная runtime-настройка конкретного запуска.

---

## Разделители

| Разделитель | После него идёт |
|-------------|-----------------|
| `://` | начало полезной нагрузки после схемы `olcrtc` |
| `?` | `<Transport>` |
| `<...>` | payload параметров транспорта |
| `@` | `<RoomID>` |
| `#` | `<EncryptionKey>` |
| `$` | `<MIMO>` |

Рекомендуется не использовать эти символы внутри самих полей. Если клиенту это нужно, он должен ввести собственное escaping/percent-encoding правило и применять его симметрично при кодировании и декодировании.

---

## Примеры

### wbstream + datachannel (не работает в обычном guest flow)

```text
olcrtc://wbstream?datachannel@room-01#d823fa01cb3e0609b67322f7cf984c4ee2e4ce2e294936fc24ef38c9e59f4799$RU / olc free sub / IPv6
```

Payload не нужен - datachannel параметров не имеет. Для WBStream этот режим **не работает** в обычном guest flow: WB Stream выдаёт токены с `canPublishData=false`, и DC не маршрутизирует данные.

### Эквивалент YAML

```yaml
mode: cnc
auth:
  provider: wbstream
room:
  id: "room-01"
crypto:
  key: "d823fa01cb3e0609b67322f7cf984c4ee2e4ce2e294936fc24ef38c9e59f4799"
net:
  transport: datachannel
data: data
```

### wbstream + vp8channel

```text
olcrtc://wbstream?vp8channel<vp8-fps=60&vp8-batch=64>@room-01#d823fa01cb3e0609b67322f7cf984c4ee2e4ce2e294936fc24ef38c9e59f4799$RU / olc free sub / IPv6
```

### Эквивалент YAML

```yaml
mode: cnc
auth:
  provider: wbstream
room:
  id: "room-01"
crypto:
  key: "d823fa01cb3e0609b67322f7cf984c4ee2e4ce2e294936fc24ef38c9e59f4799"
net:
  transport: vp8channel
vp8:
  fps: 30
  batch_size: 64
data: data
```

### wbstream + seichannel

```text
olcrtc://wbstream?seichannel<fps=60&batch=64&frag=900&ack-ms=2000>@room-01#d823fa01cb3e0609b67322f7cf984c4ee2e4ce2e294936fc24ef38c9e59f4799$DE / olc free sub
```

### Эквивалент YAML

```yaml
mode: cnc
auth:
  provider: wbstream
room:
  id: "room-01"
crypto:
  key: "d823fa01cb3e0609b67322f7cf984c4ee2e4ce2e294936fc24ef38c9e59f4799"
net:
  transport: seichannel
sei:
  fps: 30
  batch_size: 64
  fragment_size: 900
  ack_timeout_ms: 2000
data: data
```

### telemost + videochannel

```text
olcrtc://telemost?videochannel<video-w=1080&video-h=1080&video-fps=60&video-bitrate=5000k&video-hw=none&video-codec=qrcode>@room-01#d823fa01cb3e0609b67322f7cf984c4ee2e4ce2e294936fc24ef38c9e59f4799$MIMO
```

### Эквивалент YAML

```yaml
mode: cnc
auth:
  provider: telemost
room:
  id: "room-01"
crypto:
  key: "d823fa01cb3e0609b67322f7cf984c4ee2e4ce2e294936fc24ef38c9e59f4799"
net:
  transport: videochannel
video:
  width: 1080
  height: 1080
  fps: 30
  bitrate: "5000k"
  hw: none
  codec: qrcode
data: data
```

---

### jitsi + datachannel

```text
olcrtc://jitsi?datachannel@https://meet.example.org/myroom#d823fa01cb3e0609b67322f7cf984c4ee2e4ce2e294936fc24ef38c9e59f4799$RU / olc free sub
```

`<RoomID>` для jitsi - полный URL комнаты в формате `https://host/room` (или `host/room`). Поддерживается любой self-hosted Jitsi Meet инстанс без аутентификации; публичные инстансы - в [`docs/examples/jitsi.instances.yaml`](./examples/jitsi.instances.yaml) (или `meet.jit.si`). **Обязательно проверьте, какой сервер доступен в вашей сети.**

### Эквивалент YAML

```yaml
mode: cnc
auth:
  provider: jitsi
room:
  # Инстансы: docs/examples/jitsi.instances.yaml
  id: "https://meet.example.org/myroom"
crypto:
  key: "d823fa01cb3e0609b67322f7cf984c4ee2e4ce2e294936fc24ef38c9e59f4799"
net:
  transport: datachannel
data: data
```

---

## Короткие алиасы

Как хотите но лично я был бы против.

---

Формат подписки (список серверов): [sub.md](sub.ru.md)

Матрица совместимости auth + transport: [settings.md](settings.ru.md)
