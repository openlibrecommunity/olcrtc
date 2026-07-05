<div align="center">

<img src="https://github.com/openlibrecommunity/material/blob/master/olcrtc.png" width="250" height="250">

![License](https://img.shields.io/badge/license-WTFPL-0D1117?style=flat-square&logo=open-source-initiative&logoColor=green&labelColor=0D1117)
![Golang](https://img.shields.io/badge/-Golang-0D1117?style=flat-square&logo=go&logoColor=00A7D0)

[RU](settings.ru.md) / **EN**

</div>


# Settings

> **Important:** always check whether the video call service you need is on the allow lists. If it is not there, use another one. A list of all allow-listed services will be published soon.

## Compatibility matrix

| Transport | telemost | wbstream | jitsi |
|-----------|:--------:|:--------:|:-----:|
| datachannel | - | ~ | + |
| vp8channel | + | + | ~ |
| seichannel | - | + | ~ |
| videochannel | + | + | ~ |

**Legend:**
- `+` - works (passes E2E tests)
- `-` - does not work / not supported (fails E2E tests)
- `~` - unstable (may work)

**Telemost:** only vp8channel passes stably. DataChannel was removed from Telemost. seichannel is not supported. videochannel is slow.

**WBStream:** all transports except datachannel work. DataChannel does not work in the normal guest flow without being granted moderator - WB Stream issues tokens with `canPublishData=false`, and DC does not route data. To use `datachannel` over `wbstream`, set `auth.token` to an account/moderator token (`canPublishData=true`); see `auth.token` in the optional fields below.

**Jitsi:** datachannel passes stably - it is implemented on top of the colibri-ws bridge channel and sends bytes via an `EndpointMessage{raw}` broadcast. It fits self-hosted and public Jitsi Meet instances without authentication (`https://meet.small-dm.ru/...`, `https://meet1.arbitr.ru/...`, `https://meet.handyweb.org/...`, `https://meet.jit.si/...`, etc.). Check in a browser which of the servers is reachable in your network. Video transports (vp8channel, seichannel, videochannel) expose a sendable VideoTrack through the pion PeerConnection after the Jingle session-accept, but Jicofo requires additional protocol steps (LastN, ReceiverVideoConstraints, source-add) to route video - that is why they are marked `~`.

**Jitsi + seichannel - a separate caveat.** SEI NAL units ride along inside the H.264 video stream, and Jicofo on self-hosted instances (for example `meet.small-dm.ru`, `meet1.arbitr.ru`) periodically cuts/delays upstream video when there is formally no receiver in the room - for us this looks like a `seichannel ack timeout` while the PeerConnection is formally alive. In steady state the transport works, but the e2e matrix marks it `Unstable` (it flaps): a green or red result in CI is enough, the test suite does not fail on it. For reliable data transfer over jitsi, prefer `datachannel` or `vp8channel`.

**Recommended combination: `jitsi + datachannel`** - works stably on any self-hosted or public Jitsi Meet (for example `meet.small-dm.ru`, `meet1.arbitr.ru` or `meet.handyweb.org` - check which one is reachable in your network), needs no registration, simple room creation. Alternative: `wbstream + vp8channel` - stable for commercial scenarios, needs no special rights.

Speed in descending order: `datachannel` > `vp8channel` > `seichannel` > `videochannel`

---

## Required YAML config fields

| YAML field | What to enter |
|-----------|-------------|
| `mode` | `srv` on the server, `cnc` on the client, `gen` to generate a Room ID |
| `auth.provider` | `telemost`, `wbstream`, `jitsi` or `none` |
| `net.transport` | `datachannel`, `vp8channel`, `seichannel` or `videochannel` |
| `room.id` | Room ID |
| `crypto.key` or `crypto.key_file` | Encryption key, hex 64 chars. Generate: `openssl rand -hex 32` |
| `data` | Always `data` |
| `net.dns` | DNS server, e.g. `8.8.8.8:53` |

---

## Optional fields

| YAML field | Description |
|-----------|----------|
| `debug` | `true` for verbose connection logs |
| `auth.token` | Pre-issued account token for `wbstream`. When set, the session joins as that account instead of an anonymous guest; empty uses the guest flow. In the guest flow the obtained token is logged once so it can be copied back into this field to keep the same identity. Practical effect for `datachannel`: a guest token carries `canPublishData=false`, so the SCTP data channel opens but routes no bytes (the tunnel is up and silent); an account token with moderator rights carries `canPublishData=true` and routes data normally. So `datachannel` over `wbstream` requires an `auth.token` with publish rights; on the guest flow use `vp8channel`, `seichannel` or `videochannel` instead. To grant moderator: open the participants list, then the three dots next to the client/server entry, then the `Moderator` button (needed on both sides) |
| `profiles` | List of failover profiles for `srv`/`cnc` |
| `failover.retry_delay` | Pause before the next profile, e.g. `2s` |
| `failover.max_cycles` | How many full passes over the profiles to make; `0` = unlimited |
| `liveness.interval` | Ping interval over the control stream, default `10s` |
| `liveness.timeout` | How long to wait for a pong, default `5s` |
| `liveness.failures` | How many pongs may be missed before a rebuild, default `3` |
| `lifecycle.max_session_duration` | Planned session rebuild after the given time, e.g. `6h`; if unset, disabled |
| `traffic.max_payload_size` | Limit on the encrypted wire-message size; `0` = transport limit |
| `traffic.min_delay` / `.max_delay` | Optional send pacing, e.g. `5ms` / `30ms` |

`crypto.key_file` is read relative to the YAML file. Do not set `crypto.key` and `crypto.key_file` at the same time.

If `profiles` is set, the top-level fields become shared defaults, and each
profile overrides only its own `auth`, `room`, `net`, `engine` and
transport/liveness settings. The profile order must match on server and client.

`liveness` checks the encrypted smux control stream after the handshake,
not just the WebRTC/provider connection status. If a pong does not arrive
several times in a row, the current smux session is rebuilt.

`lifecycle.max_session_duration` limits the duration of a single call /
provider session. When the timer expires, the current `srv` or `cnc` session
closes and starts again with the same config. While this setting is enabled,
a clean session end is also restarted so the second peer can catch up with the
planned rebuild. Value format: `30m`, `2h`, `6h`; `0s` and negative values are
not accepted.

`traffic` adds a common wrapper over the chosen transport. It can limit the
encrypted message size and add a small delay before sending. Data is not
truncated: if a message does not fit the effective limit, send returns an
explicit error. When `max_payload_size` is set, the smux frame size is also
reduced by the crypto overhead; with `0` the chosen transport limit remains.
Use the same traffic settings on both sides.

---

## mode: gen

`gen` is kept for auth providers that can create rooms through an API.
Currently the built-in providers do not support room auto-creation through `olcrtc`.

For `telemost` and `wbstream`, create a room through the service site and paste
its ID into `room.id`. For `jitsi`, specify the room URL.

---

## Server-only fields (`mode: srv`)

| YAML field | Description |
|-----------|----------|
| `socks.proxy_addr` | Address of the SOCKS5 proxy for the server outbound traffic |
| `socks.proxy_port` | Port of that proxy |
| `socks.proxy_user` | Login for upstream-proxy authentication (optional) |
| `socks.proxy_pass` | Password for upstream-proxy authentication (optional) |

If `socks.proxy_user` is empty, the server reaches the proxy without authentication (method `0x00`).
If it is set, username/password auth per RFC 1929 is used (`proxy_pass` is optional and may be empty).

---

## Client-only fields (`mode: cnc`)

| YAML field | Description | Default |
|-----------|----------|:------------:|
| `socks.host` | Which address to start SOCKS5 on | `127.0.0.1` |
| `socks.port` | Which port to start SOCKS5 on | `1080` |
| `socks.user` | Login for incoming SOCKS5 connections (optional) | - |
| `socks.pass` | Password for incoming SOCKS5 connections (optional) | - |

If `socks.user` is not set, authentication is disabled (any local client may connect).  
If it is set, the client accepts only connections with the correct login and password (RFC 1929).

If `socks.host` is not loopback (`127.0.0.1`, `::1`, `localhost`), `socks.user` and `socks.pass` are required.
This protects against accidentally opening a SOCKS5 proxy on the local network or the internet.

---

## datachannel

No extra fields - everything is default.

---

## vp8channel

**Recommended: `fps: 30`, `batch_size: 64`** (lower FPS reduces CPU load, larger batch = higher speed)

| YAML field | Description | Default |
|-----------|----------|:------------:|
| `vp8.fps` | VP8 stream FPS | `30` |
| `vp8.batch_size` | Frames per tick | `64` |

---

## seichannel

**Recommended: `fps: 30`, `batch_size: 64`, `fragment_size: 900`, `ack_timeout_ms: 2000`**

| YAML field | Description | Default |
|-----------|----------|:------------:|
| `sei.fps` | H264 stream FPS | `30` |
| `sei.batch_size` | Frames per tick | `64` |
| `sei.fragment_size` | Fragment size in bytes | `900` |
| `sei.ack_timeout_ms` | ACK timeout in milliseconds | `2000` |

---

## videochannel

**Recommended: `codec: qrcode`, `width: 1080`, `height: 1080`, `fps: 30`, `bitrate: "5000k"`, `hw: none`**

| YAML field | Description | Default |
|-----------|----------|:------------:|
| `video.codec` | `qrcode` or `tile` | `qrcode` |
| `video.width` | Width in pixels | `1920` |
| `video.height` | Height in pixels | `1080` |
| `video.fps` | FPS | `30` |
| `video.bitrate` | Bitrate, e.g. `"2M"` or `"5000k"` | `"2M"` |
| `video.hw` | Hardware acceleration: `none` or `nvenc` | `none` |
| `video.qr_recovery` | QR error correction: `low` / `medium` / `high` / `highest` | `low` |
| `video.qr_size` | QR fragment size in bytes, `0` = auto | `0` |
| `video.tile_module` | Tile size in pixels 1..270 (`tile` only) | `4` |
| `video.tile_rs` | Reed-Solomon parity % 0..200 (`tile` only) | `20` |
| `ffmpeg` | Path to the ffmpeg executable | `ffmpeg` |

For codec `tile` exactly `1080x1080` is required.

---

## Ready-made configs

### wbstream + datachannel (does not work in the normal guest flow)

WB Stream DataChannel **does not work** in the normal guest flow - WB Stream issues tokens with `canPublishData=false`, and DC does not route data. This mode is marked as expected fail in E2E tests. For normal use pick `vp8channel`, `seichannel` or `videochannel`.

To make `datachannel` work you need an account/moderator token in `auth.token` (`canPublishData=true`) on both sides. To grant moderator in the WB Stream UI: open the participants list

![participants list](asset/wbstream-moderator/participants.png)

click the three dots next to the client/server entry

![entry menu](asset/wbstream-moderator/menu.png)

then press the `Moderator` button

![moderator button](asset/wbstream-moderator/moderator-button.png)

> Future work: when joining with a ghost/account token that already holds
> moderator rights, the client could be auto-promoted to moderator so
> `datachannel` works without manual promotion. Not implemented yet;
> moderator is still required on both sides manually.

```yaml
# the room ID must be created manually via https://stream.wb.ru

# server.yaml
mode: srv
auth:
  provider: wbstream
room:
  id: "<room-id-from-stream.wb.ru>"
crypto:
  key: "<hex-key>"
net:
  transport: datachannel
  dns: "8.8.8.8:53"
data: data
```

```yaml
# client.yaml
mode: cnc
auth:
  provider: wbstream
room:
  id: "<room-id-from-stream.wb.ru>"
crypto:
  key: "<hex-key>"
net:
  transport: datachannel
  dns: "8.8.8.8:53"
socks:
  host: "127.0.0.1"
  port: 8808
data: data
```

### wbstream + datachannel + SOCKS5 authentication (does not work in the normal guest flow)

```yaml
# client.yaml with proxy login and password
mode: cnc
auth:
  provider: wbstream
room:
  id: "<room-id>"
crypto:
  key: "<hex-key>"
net:
  transport: datachannel
  dns: "8.8.8.8:53"
socks:
  host: "127.0.0.1"
  port: 8808
  user: myuser
  pass: mypass
data: data
```

Usage:
```sh
curl --socks5-hostname myuser:mypass@127.0.0.1:8808 https://icanhazip.com
# or
export all_proxy=socks5h://myuser:mypass@127.0.0.1:8808
```

---

### telemost + vp8channel

```yaml
# server.yaml
mode: srv
auth:
  provider: telemost
room:
  id: "<room-id>"
crypto:
  key: "<hex-key>"
net:
  transport: vp8channel
  dns: "8.8.8.8:53"
vp8:
  fps: 30
  batch_size: 64
data: data
```

```yaml
# client.yaml
mode: cnc
auth:
  provider: telemost
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
vp8:
  fps: 30
  batch_size: 64
data: data
```

### telemost + seichannel (does not work)

> ⚠️ This combination is marked as expected fail in E2E tests. Telemost does not support seichannel.

```yaml
# server.yaml
mode: srv
auth:
  provider: telemost
room:
  id: "<room-id>"
crypto:
  key: "<hex-key>"
net:
  transport: seichannel
  dns: "8.8.8.8:53"
sei:
  fps: 30
  batch_size: 64
  fragment_size: 900
  ack_timeout_ms: 2000
data: data
```

```yaml
# client.yaml
mode: cnc
auth:
  provider: telemost
room:
  id: "<room-id>"
crypto:
  key: "<hex-key>"
net:
  transport: seichannel
  dns: "8.8.8.8:53"
socks:
  host: "127.0.0.1"
  port: 8808
sei:
  fps: 30
  batch_size: 64
  fragment_size: 900
  ack_timeout_ms: 2000
data: data
```

### telemost + videochannel (best effort, unstable)

```yaml
# server.yaml
mode: srv
auth:
  provider: telemost
room:
  id: "<room-id>"
crypto:
  key: "<hex-key>"
net:
  transport: videochannel
  dns: "8.8.8.8:53"
video:
  codec: qrcode
  width: 1080
  height: 1080
  fps: 30
  bitrate: "5000k"
  hw: none
data: data
```

```yaml
# client.yaml
mode: cnc
auth:
  provider: telemost
room:
  id: "<room-id>"
crypto:
  key: "<hex-key>"
net:
  transport: videochannel
  dns: "8.8.8.8:53"
socks:
  host: "127.0.0.1"
  port: 8808
video:
  codec: qrcode
  width: 1080
  height: 1080
  fps: 30
  bitrate: "5000k"
  hw: none
data: data
```

---

More on running: [Quick start](fast.md) · [Manual build](manual.md)

URI format for clients: [uri.md](uri.md) · [Subscription format](sub.md)
