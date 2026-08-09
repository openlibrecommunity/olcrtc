<div align="center">

<img src="https://github.com/openlibrecommunity/material/blob/master/olcrtc.png" width="250" height="250">

![License](https://img.shields.io/badge/license-WTFPL-0D1117?style=flat-square&logo=open-source-initiative&logoColor=green&labelColor=0D1117)
![Golang](https://img.shields.io/badge/-Golang-0D1117?style=flat-square&logo=go&logoColor=00A7D0)

[RU](uri.ru.md) / **EN**

</div>


# Compact URI format for clients

This document describes a **convention for developers of client applications** that need a compact way to pass `olcrtc` connection parameters.

The current `olcrtc` does not parse such a URI automatically. If a client application wants to use this notation, it must parse the string itself and pass the resulting fields into the `olcrtc` YAML config.

The main client that consumes this URI format is [owenewans/owenclave](https://github.com/owenewans/owenclave) ([src.owenewans.org/owenrtc](https://src.owenewans.org/owenrtc)) - an Android proxy client (fork of exclave) supporting all common protocols (vless, hysteria2, mieru, trojan, vmess, tuic, shadowsocks, socks ...) plus `olcrtc` and subscriptions.

---

## Format

```text
olcrtc://<Auth>?<Transport>@<RoomID>#<EncryptionKey>$<MIMO>
olcrtc://<Auth>?<Transport><key=value&key=value>@<RoomID>#<EncryptionKey>$<MIMO>
```

Everything after `olcrtc://` is considered part of the client convention.

The `<key=value&...>` block is the transport parameter payload in angle brackets, placed right after the transport name. If the transport needs no parameters or uses defaults, the block is dropped entirely.

---

## Fields

| Field | Meaning |
|------|----------|
| `<Auth>` | Auth provider name, e.g. `telemost`, `wbstream`, `jitsi` |
| `<Transport>` | Transport name, e.g. `datachannel`, `vp8channel`, `seichannel`, `videochannel` |
| payload | Transport parameters in `<key=value&...>`. Keys match the YAML fields. The block is dropped when defaults are used |
| `<RoomID>` | Room identifier or auth-specific room URL/ID |
| `<EncryptionKey>` | Encryption key in hex, usually 64 chars (`32` bytes) |
| `<MIMO>` | Free-form comment for UI/metadata, e.g. `RU / olc free sub / IPv6` |

---

## Payload parameters per transport

### datachannel

No payload.

### vp8channel

| Key | YAML field | Description |
|------|-----------|----------|
| `vp8-fps` | `vp8.fps` | VP8 stream FPS |
| `vp8-batch` | `vp8.batch_size` | Frames per tick |

### seichannel

| Key | YAML field | Description |
|------|-----------|----------|
| `fps` | `sei.fps` | H264 stream FPS |
| `batch` | `sei.batch_size` | Frames per tick |
| `frag` | `sei.fragment_size` | Fragment size in bytes |
| `ack-ms` | `sei.ack_timeout_ms` | ACK timeout in milliseconds |

### videochannel

| Key | YAML field | Description |
|------|-----------|----------|
| `video-w` | `video.width` | Width in pixels |
| `video-h` | `video.height` | Height in pixels |
| `video-fps` | `video.fps` | FPS |
| `video-bitrate` | `video.bitrate` | Bitrate, e.g. `5000k` or `2M` |
| `video-hw` | `video.hw` | Hardware acceleration: `none` or `nvenc` |
| `video-codec` | `video.codec` | `qrcode` or `tile` |
| `video-qr-size` | `video.qr_size` | QR fragment size in bytes |
| `video-qr-recovery` | `video.qr_recovery` | Error correction: `low` / `medium` / `high` / `highest` |
| `video-tile-module` | `video.tile_module` | Tile size in pixels 1..270 (`tile` only) |
| `video-tile-rs` | `video.tile_rs` | Reed-Solomon parity % 0..200 (`tile` only) |

---

## Mapping to olcrtc YAML fields

| URI field | YAML field |
|----------|-----------|
| `<Auth>` | `auth.provider` |
| `<Transport>` | `net.transport` |
| payload | matching transport YAML fields |
| `<RoomID>` | `room.id` |
| `<EncryptionKey>` | `crypto.key` |
| `<MIMO>` | Not passed to `olcrtc`. Client comment only |

`data: data` is not encoded in this format, because it is a local runtime setting of a specific run.

---

## Separators

| Separator | What follows it |
|-------------|-----------------|
| `://` | start of the payload after the `olcrtc` scheme |
| `?` | `<Transport>` |
| `<...>` | transport parameter payload |
| `@` | `<RoomID>` |
| `#` | `<EncryptionKey>` |
| `$` | `<MIMO>` |

It is recommended not to use these characters inside the fields themselves. If a client needs to, it must introduce its own escaping/percent-encoding rule and apply it symmetrically when encoding and decoding.

---

## Examples

### wbstream + datachannel (does not work in the normal guest flow)

```text
olcrtc://wbstream?datachannel@room-01#d823fa01cb3e0609b67322f7cf984c4ee2e4ce2e294936fc24ef38c9e59f4799$RU / olc free sub / IPv6
```

No payload is needed - datachannel has no parameters. For WBStream this mode **does not work** in the normal guest flow: WB Stream issues tokens with `canPublishData=false`, and DC does not route data.

### YAML equivalent

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

### YAML equivalent

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

### YAML equivalent

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

### YAML equivalent

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

`<RoomID>` for jitsi is the full room URL in the form `https://host/room` (or `host/room`). Any self-hosted Jitsi Meet instance without authentication is supported; for public instances see [`docs/examples/jitsi.instances.yaml`](./examples/jitsi.instances.yaml) (or `meet.jit.si`). **Be sure to check which server is reachable in your network.**

### YAML equivalent

```yaml
mode: cnc
auth:
  provider: jitsi
room:
  # Instances: see docs/examples/jitsi.instances.yaml
  id: "https://meet.example.org/myroom"
crypto:
  key: "d823fa01cb3e0609b67322f7cf984c4ee2e4ce2e294936fc24ef38c9e59f4799"
net:
  transport: datachannel
data: data
```

---

## Short aliases

Do as you wish, but personally I would be against it.

---

Subscription format (server list): [sub.md](sub.md)

Compatibility matrix for auth + transport: [settings.md](settings.md)
