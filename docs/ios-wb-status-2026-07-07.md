# iOS WB status, 2026-07-07

This note records the current physical-device verification for the WB carrier
path on iOS. All raw logs that may contain runtime identifiers remain under
`.secrets/runtime/`; sanitized artifacts are under the repo-local artifact
directory listed below.

## Build under test

- Branch: `ios-wb-ready`
- Base: `udp-associate-relatch`, head `0219faa`
- Device class: iPhone 11, physical device
- App profile under test: `wb`
- Carrier: `wbstream`
- Transport: `vp8channel`

The branch adds the mobile readiness API needed by the iOS Network Extension:

- `session.RunWithReady(ctx, cfg, onReady)`
- `olcmobile.WaitReady(timeoutMillis)`

The readiness callback is fired after the CNC session and local SOCKS listener
are ready, which lets the iOS tunnel avoid publishing Network Extension routing
before the data path exists.

## WB tunnel probe result

Artifact root:

`/Users/oxi/unite/whitelist-bypass/artifacts/wb-ios/harness/ios-wb-device-20260707-174053/`

Files:

- `verdict.json`
- `ios-logs/app-probes-summary.log`
- `ios-logs/cnc-summary.log`
- `logs/server-summary.log`

Verdict:

```json
{
  "Green": true,
  "RoundOK": 3,
  "DownloadOK": 3,
  "HTTPError": 0,
  "Reconnects": 0,
  "TURNAllocation401": 6,
  "DownloadDurationsMs": [9315, 10017, 8860]
}
```

The iOS app completed three probe rounds through the active Network Extension:

- `api.ipify.org`
- `example.com`
- `speed.cloudflare.com` 1 MiB download per round

All three 1 MiB downloads completed. The WB control channel stayed alive and
the server did not report reconnects or teardown during the probe window.

## Telegram iOS smoke

Telegram was tested as a real iOS app under the same active WB VPN. The test
forced a clean foreground launch of bundle `ph.telegra.Telegraph` with a
`tg://resolve?...` payload so the app rebuilt its network connections after the
VPN was already up.

Files:

- `logs/telegram-ios-launch.txt`
- `logs/server-telegram-ios-summary-latest.log`
- `logs/telegram-ios-verdict.json`
- `ios-logs-after-telegram/cnc-stderr.log`

Verdict:

```json
{
  "Green": true,
  "TelegramConnects": 55,
  "TelegramTrafficLines": 41,
  "TelegramUniqueIPs": [
    "149.154.162.123",
    "149.154.167.255",
    "149.154.167.35",
    "149.154.167.41",
    "149.154.167.51",
    "149.154.175.211"
  ],
  "TelegramBytesIn": 99236,
  "TelegramBytesOut": 6230219,
  "ControlAliveLines": 11,
  "ErrorLikeLines": 0
}
```

This verifies that Telegram traffic from the physical iPhone traversed the WB
tunnel. The iOS tunnel logs also continued emitting `tun2socks stats` after the
Telegram launch, with counters increasing during the test window.

## Longer-run follow-up

A later live-tail check found a reconnect after the initial green WB and
Telegram smoke window. Server evidence:

- `2026-07-07 15:08:43 UTC`: first `control missed pong on server`
- `2026-07-07 15:09:13 UTC`: `control stream unhealthy`, then
  `server reconnect reason=liveness`
- The peer session closed with duration `26m30s`
- `2026-07-07 15:09:15-15:09:16 UTC`: carrier reconnect/ICE connected started

Client app-group logs copied after the reconnect only contained tunnel output
through `2026-07-07 15:07:44 UTC`, about 89 seconds before the server liveness
failure. The copied client logs did not contain post-reconnect recovery evidence.

Conclusion: WB is verified for startup, three HTTP/download probe rounds, and a
Telegram foreground smoke on physical iOS, but it is not yet certified as
long-running stable under real background app load. The next root-cause target
is the liveness/control path under sustained Telegram/Mail socket churn, plus
why iOS tunnel logging stopped before the server declared the control stream
unhealthy.

## Telemost status

Telemost is not green on physical iOS in the latest controlled runs:

- `ios-telemost-20260707-162556`: `RoundOK=1/3`, `DownloadOK=1/3`,
  `HTTPError=1`, `Reconnects=3`
- `ios-telemost-cleanproc-20260707-165740`: `RoundOK=2/3`,
  `DownloadOK=2/3`, `HTTPError=1`, `Reconnects=5`

The WB readiness work in this branch does not claim to fix Telemost. Telemost
still needs separate root-cause work.

## iOS app DNS note

The iOS app currently lives outside this git repository in this workspace. A
runtime fix was applied there so `wbstream` renders a single DNS resolver:
`77.88.8.8:53`.

Why: the WB branch resolver expects one `host:port`. Rendering
`8.8.8.8:53,192.168.1.1:53` caused CNC startup to fail before the data path with
`too many colons in address`.

When the iOS app source is moved into a git repository, keep this behavior:

```swift
let dnsServer = s.carrier == "wbstream" ? "77.88.8.8:53" : "8.8.8.8:53"
```

and render:

```yaml
dns: "\(dnsServer)"
```
