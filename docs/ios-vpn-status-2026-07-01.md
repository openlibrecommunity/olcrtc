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

Current conclusion: Telemost signaling reaches "connected", but the VP8 data
transport does not become ready for tunnel payload. Continue debugging at the
Telemost/WebRTC data-plane boundary, not at iOS VPN routing.
