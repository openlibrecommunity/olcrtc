# iOS WB status, 2026-07-07

This note records the current physical-device verification for the WB carrier
path on iOS. All raw logs that may contain runtime identifiers remain under
`.secrets/runtime/`; sanitized artifacts are under the repo-local artifact
directory listed below.

## Build under test

- Mergeable branch: `ios-wb-ready-slim`
- Base: `origin/master`, head `ad57585`
- Branch commits:
  - `49a99d9 fix: expose mobile readiness for iOS WB`
  - `HEAD fix: enforce local SOCKS block policy`
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

## Local SOCKS block policy fix

The later long-run failure was reproduced with high background socket churn from
Mail/APNs while Telegram was foregrounded. The iOS app rendered:

```yaml
socks:
  block_ports: [993, 5223]
  block_hosts: ["*.apple.com", "*.icloud.com", "*.cdn-apple.com"]
  block_cidrs: ["17.0.0.0/8"]
```

but the Go runtime did not parse or enforce these fields. As a result, IMAP
(`:993`) and Apple push/iCloud sockets were still opening smux streams through
the WB carrier and could starve the control path under long-running load.

This branch now maps those YAML fields through `internal/config` and
`internal/app/session`, validates ports/CIDRs, and enforces the policy inside the
local SOCKS handler before a tunnel stream is opened. TCP `CONNECT` requests get
a SOCKS host-unreachable reply. The current upstream branch does not include the
old SOCKS UDP-associate datapath, so the mergeable slim branch intentionally does
not reintroduce it.

The policy is intentionally local to CNC/SOCKS. It does not require server
changes and it protects any client platform that routes through the olcrtc SOCKS
listener, including the iOS Network Extension.

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

Conclusion before the local SOCKS policy fix: WB was verified for startup,
three HTTP/download probe rounds, and a Telegram foreground smoke on physical
iOS, but it was not certified as long-running stable under real background app
load. This finding is superseded by the policy-fix probe below.

## WB policy-fix probe, 2026-07-07

Artifact root:

`/Users/oxi/unite/whitelist-bypass/artifacts/wb-ios/harness/ios-wb-policy-20260707-190937/`

Files:

- `logs/policy-final-verdict.json`
- `logs/policy-final-summary.txt`
- `logs/policy-smoke-summary.txt`
- `logs/longrun-monitor-targets.txt`
- `logs/ios-logs-final/app.log`
- `logs/ios-logs-final/cnc-stderr.log`
- `logs/server-journal-final.log`
- `logs/telegram-policy-launch.json`

Short smoke after rebuilding `OlcMobile.xcframework` from this branch:

- VPN ready at `2026-07-07 16:10:25 UTC`
- HTTP probe loop: `RoundOK=3/3`, `DownloadOK=3/3`, `HTTPError=0`
- 1 MiB download durations: `10843 ms`, `9763 ms`, `8881 ms`
- Server journal: `mail_or_apple_lines=0`
- Server journal: Telegram lines present during foreground Telegram smoke
- Client log: local policy blocked Mail/APNs/iCloud before tunnel stream open,
  including `mail.digitaldealingdesk.eu:993`,
  `2-courier.push.apple.com:5223`, `17.57.146.138:443`, and
  `probe.icloud.com:443`

Final long-run verdict:

```json
{
  "Green": true,
  "StartedUTC": "2026-07-07 16:10:22 UTC",
  "EndedUTC": "2026-07-07 16:48:32 UTC",
  "LongRunSeconds": 2290,
  "RoundOK": 3,
  "DownloadOK": 3,
  "DownloadDurationsMs": [10843, 9763, 8881],
  "LivenessOrReconnect": 0,
  "MailOrAppleServerTargetLines": 0,
  "TelegramServerTargetLines": 3730,
  "PeerConnectedLines": 1,
  "LocalSOCKSBlockedMaxLoggedCount": 2100,
  "SanitizedLeakCheckLines": 0
}
```

The policy-fix build stayed up for `38m10s`, crossing the earlier `26m30s`
failure point. The server journal contains no `control missed pong`, `control
stream unhealthy`, reconnect, teardown, or server-side Mail/Apple target lines
for the test window. Telegram was foreground-launched on the iPhone during the
same run and continued producing server-side target traffic through the WB
tunnel.

## Mergeable slim-branch probe, 2026-07-07

The upstream PR originally carried older history and conflicted with current
`origin/master`. To make the PR mergeable, the two iOS/WB fixes were replayed on
top of `origin/master` as `ios-wb-ready-slim`.

Artifact root:

`/Users/oxi/unite/whitelist-bypass/artifacts/wb-ios/harness/ios-wb-slim-20260707-171335/`

Files:

- `logs/slim-after-deploy-verdict.json`
- `logs/slim-after-deploy-summary.txt`
- `logs/server-after-deploy-sanitized.log`
- `ios-logs-after-deploy-sanitized/app.log`
- `ios-logs-after-deploy-sanitized/tunnel.log`
- `ios-logs-after-deploy-sanitized/cnc-stderr.log`

Important setup note: the first slim iOS retry against the previously deployed
server binary failed with `vp8channel: incoming frame bad header len=36`, which
was a client/server binary mismatch. After building `./cmd/olcrtc` from the
same slim branch and deploying it to `/opt/olc-bypass/bin/olcrtc`,
`olc-wb-srv` was restarted and the physical iOS probe was green.

Verification:

- `go test -count=1 ./...`: passed
- `gomobile bind -target=ios,iossimulator ./mobile/olcmobile`: passed
- exported mobile symbols present: `OlcmobileStartCnc`, `OlcmobileWaitReady`,
  `OlcmobileStop`
- physical iPhone 11 WB VPN probe: passed

Slim verdict:

```json
{
  "Green": true,
  "StartedUTC": "2026-07-07 17:26:33 UTC",
  "EndedUTC": "2026-07-07 17:29:26 UTC",
  "RoundOK": 3,
  "IpifyOK": 3,
  "ExampleOK": 3,
  "DownloadOK": 3,
  "DownloadDurationsMs": [8690, 9662, 9433],
  "LivenessReconnectBadHeaderLines": 0,
  "MailOrAppleServerTargetLines": 0,
  "ServerPeerConnectedLines": 1,
  "ServerControlAliveLines": 17,
  "SanitizedLeakCheckLines": 0
}
```

This is a short mergeability/regression probe for the rebased slim branch. The
longer `38m10s` stability evidence remains the policy-fix run above.

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
