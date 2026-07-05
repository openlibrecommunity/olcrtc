<div align="center">

<img src="https://github.com/openlibrecommunity/material/blob/master/olcrtc.png" width="250" height="250">

![License](https://img.shields.io/badge/license-WTFPL-0D1117?style=flat-square&logo=open-source-initiative&logoColor=green&labelColor=0D1117)
![Golang](https://img.shields.io/badge/-Golang-0D1117?style=flat-square&logo=go&logoColor=00A7D0)

[RU](configuration.ru.md) / **EN**

</div>


# YAML configuration

`olcrtc` reads runtime settings from a single YAML file. The CLI takes exactly one argument - the path to the config; there are no separate CLI flags for mode, transport and provider anymore.

```bash
olcrtc /etc/olcrtc/server.yaml
olcrtc /etc/olcrtc/client.yaml
```

Ready-made examples:

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

## Schema

| YAML path | Meaning |
|---|---|
| `mode` | `srv`, `cnc` or `gen` |
| `auth.provider` | `jitsi`, `telemost`, `wbstream`, `none` |
| `room.id` | room ID/URL for the chosen auth provider |
| `room.channel` | optional channel ID for peer-routing scenarios |
| `crypto.key` / `crypto.key_file` | shared key: 64 hex chars, directly or from a file |
| `net.transport` | `datachannel`, `vp8channel`, `seichannel`, `videochannel` |
| `net.dns` | DNS resolver in `host:port` form |
| `socks.host` / `socks.port` | local SOCKS5 listener in `mode: cnc` |
| `socks.user` / `socks.pass` | optional auth for incoming SOCKS5 connections |
| `socks.proxy_addr` / `socks.proxy_port` | outbound SOCKS5 proxy on the server side |
| `socks.proxy_user` / `socks.proxy_pass` | optional auth for the upstream proxy (RFC 1929) |
| `engine.name` / `engine.url` / `engine.token` | direct engine mode, only when `auth.provider: none` |
| `video.*` | `videochannel` settings |
| `vp8.*` | `vp8channel` settings |
| `sei.*` | `seichannel` settings |
| `liveness.interval` | ping interval over the control stream, default `10s` |
| `liveness.timeout` | pong timeout, default `5s` |
| `liveness.failures` | how many pongs may be missed before rebuild, default `3` |
| `lifecycle.max_session_duration` | planned session rebuild, e.g. `6h`; empty = disabled |
| `traffic.max_payload_size` | limit of the encrypted wire-message; `0` = transport limit |
| `traffic.min_delay` / `traffic.max_delay` | optional send pacing, e.g. `5ms` / `30ms` |
| `gen.amount` | `gen` mode: how many rooms to create |
| `profiles[]` | list of failover profiles for `srv`/`cnc` |
| `failover.retry_delay` | pause before the next profile, e.g. `2s` |
| `failover.max_cycles` | how many full passes over the profiles to do; `0` = infinite |
| `data` | path to the directory with runtime data (`names`, `surnames`) |
| `debug` | verbose logging |
| `ffmpeg` | path to the ffmpeg binary for `videochannel` |

`crypto.key_file` is read relative to the YAML file. You cannot set `crypto.key` and `crypto.key_file` at the same time.

`mode: cnc` forbids listening on a non-loopback address (`0.0.0.0`, LAN IP etc.) unless both `socks.user` and `socks.pass` are set.

## Required minimum

### Server

> **Jitsi provider:** use the server that is reachable in your network. Check in the browser and pick a working one:
> - `https://meet.small-dm.ru/`
> - `https://meet1.arbitr.ru/` 
> - `https://meet.handyweb.org/`

```yaml
mode: srv
auth:
  provider: jitsi
room:
  # Use the Jitsi server that works in your network:
  # https://meet.small-dm.ru/ROOM  or  https://meet1.arbitr.ru/ROOM  or  https://meet.handyweb.org/ROOM
  id: "https://meet.small-dm.ru/REPLACE_ME_WITH_ROOM_ID"
crypto:
  key: "REPLACE_ME_WITH_64_HEX_CHARS"
net:
  transport: datachannel
  dns: "8.8.8.8:53"
data: data
```

### Client

```yaml
mode: cnc
auth:
  provider: jitsi
room:
  # Use the Jitsi server that works in your network:
  # https://meet.small-dm.ru/ROOM  or  https://meet1.arbitr.ru/ROOM  or  https://meet.handyweb.org/ROOM
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

After `CLIENT_HELLO` / `SERVER_WELCOME` the first smux stream stays open as an encrypted control stream. Over it `olcrtc` sends `CONTROL_PING` / `CONTROL_PONG` to check the actually working tunnel path, not just the status of the WebRTC connection.

```yaml
liveness:
  interval: 10s
  timeout: 5s
  failures: 3
```

When the threshold of missed pongs is reached, the current smux session is rebuilt. In failover mode the profile that finished after a failed reconnect hands control to the supervisor, and the supervisor tries the next profile.

## Lifecycle Rotation

`lifecycle.max_session_duration` sets a planned upper bound on the duration of a single call/session at the provider. When the time runs out, the active `srv` or `cnc` session is closed and started again with the same config.

```yaml
lifecycle:
  max_session_duration: 6h
```

The field is optional. Format is Go duration: `30m`, `2h`, `6h`. Zero and negative values are not accepted.

## Traffic Shaping

`traffic` adds a common wrapper around the chosen transport. It can limit the size of the encrypted message and add a small delay before sending. Data is not truncated: if the payload does not fit the effective limit, the send fails with an explicit error.

```yaml
traffic:
  max_payload_size: 4096
  min_delay: 5ms
  max_delay: 30ms
```

The limit is clamped to the `MaxPayloadSize` declared by the chosen transport. Client and server also reduce the smux frame size accounting for crypto overhead. A value of `0` adds no limit beyond the transport limit. If only `min_delay` is set, the delay is fixed. Use the same `traffic` settings on both sides.

## Failover Profiles

`mode: srv` and `mode: cnc` can define `profiles`. Top-level fields become shared defaults, and each profile overrides only what is specified inside it.

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

The order of profiles and the room parameters must be compatible on the server and the client. Active smux streams do not migrate between profiles; new connections can recover on the next profile.

## mode: gen

`gen` is kept for auth providers that implement room creation via an API.
The current built-in providers (`jitsi`, `telemost`, `wbstream`) do not create rooms
through `olcrtc`: for `telemost` and `wbstream` create the room on the service site and
paste it into `room.id`; for `jitsi` specify the room URL.
