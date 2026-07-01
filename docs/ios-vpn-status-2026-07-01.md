# iOS VPN status, 2026-07-01

Device: physical iPhone 11 (`406AE25C-CC1F-592D-A60B-872C2D2E6427`).
Bundle: `ru.unite.olc.ios`.
App group: `group.ru.unite.olc`.

## Measurement method

Use one app launch for connect and probes. Do not terminate the app between
`--connect-on-launch` and the probe loop.

The probe loop runs:

- custom URL: `https://example.com/...`
- `https://api.ipify.org?format=json`
- `https://example.com/...`
- `https://speed.cloudflare.com/__down?bytes=1048576`

For a valid result, verify all of these:

- fresh `=== startTunnel ===`
- `SOCKS ready`
- growing `tun2socks stats`
- target host appears in `cnc-stderr.log` via SOCKS
- `OlcTunnel` process remains alive after the probe loop

## WB result

Launch:

```sh
xcrun devicectl device process launch \
  --device 406AE25C-CC1F-592D-A60B-872C2D2E6427 \
  ru.unite.olc.ios \
  --profile-id wb \
  --connect-on-launch \
  --probe-rounds 10 \
  --probe-interval 10 \
  --probe-download-bytes 1048576 \
  --http-probe-url 'https://example.com/?olc-10round-wb-current=20260701-1828'
```

Result:

- `connect start provider=wbstream transport=vp8channel`
- 10/10 rounds successful (`ok=4 fail=0`)
- 10/10 `speed.cloudflare.com:443` SOCKS sessions
- no fresh `remote not ready`, `readVP8Track closed`, or `cnc ENDED`
- `OlcClientiOS` and `OlcTunnel` stayed alive

1 MiB Cloudflare download throughput:

- average: 0.826 Mbps
- min: 0.466 Mbps
- max: 1.322 Mbps
- p50: 0.826 Mbps
- p90: 1.160 Mbps

## Telemost result

### Latest valid smoke after Telemost fixes

Launch:

```sh
xcrun devicectl device process launch \
  --device 406AE25C-CC1F-592D-A60B-872C2D2E6427 \
  ru.unite.olc.ios \
  --profile-id telemost \
  --connect-on-launch \
  --probe-rounds 4 \
  --probe-interval 5 \
  --probe-download-bytes 0 \
  --http-probe-url 'https://example.com/?olc-smoke-telemost-initialwait=20260701-2021'
```

Result:

- `profile override id=telemost`
- `connect start provider=telemost transport=vp8channel`
- `connect ok provider=telemost`
- `SOCKS ready @2500ms`
- 4/4 short HTTP probe rounds successful (`ok=3 fail=0`)
- `tun2socks stats` grew during the probe loop
- `cnc-stderr.log` showed `peer observed`, `peer confirmed`, `session opened`
- one liveness/carrier reconnect was observed and recovered with `session reopened`

The Telemost fixes in this branch:

- bind VP8 peers by `room.channel` when the profile provides one, falling back
  to the room value for legacy profiles
- do not confirm a peer from VP8 keepalives alone; confirm only after real KCP
  payload delivery
- do not stop carrier reconnect after 5 attempts for peer-aware transports
- extend initial peer wait to tolerate Telemost auth/ws reconnect delays

### Bulk download status

The previous 10-round Telemost run with `--probe-download-bytes 1048576` is not
a valid VPN success proof: the tunnel had already ended with `client: wait for
peer`, so later HTTP responses could be direct traffic. Re-running with a live
tunnel still showed that 1 MiB `speed.cloudflare.com` downloads time out on
Telemost.

Current conclusion: Telemost now starts, carries short HTTPS traffic, and can
recover from at least one observed liveness/carrier reconnect on the iOS device.
It is not yet proven stable for sustained throughput or bulk downloads.

### Earlier failing run

Launch:

```sh
xcrun devicectl device process launch \
  --device 406AE25C-CC1F-592D-A60B-872C2D2E6427 \
  ru.unite.olc.ios \
  --profile-id telemost \
  --connect-on-launch \
  --probe-rounds 10 \
  --probe-interval 10 \
  --probe-download-bytes 1048576 \
  --http-probe-url 'https://example.com/?olc-10round-telemost=20260701-1823'
```

Observed:

- `profile override id=telemost`
- `connect start provider=telemost transport=vp8channel`
- `connect ok provider=telemost`
- `SOCKS ready`
- first full probe round failed: `ok=0 fail=4`
- repeated `remote not ready` in `cnc-stderr.log`
- `tun2socks stats` grew only minimally, indicating signaling/SOCKS accept worked
  but data-plane payload did not flow

Additional caveats:

- local macOS testing was contaminated by a pre-existing utun default route,
  so local host-side direct/VPN separation is not reliable enough for final
  throughput claims
- remote VPS deployment is still blocked by SSH access/host-key/public-key
  issues; the remote stand was not updated with this branch
