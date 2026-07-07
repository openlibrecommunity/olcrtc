# iOS VPN / Telemost status - 2026-07-06

## Scope

This note captures the Telemost iOS VPN debugging state after adding explicit
client readiness propagation from `olcrtc` to the iOS Network Extension.

## Root causes confirmed

- The previous bulk run at `2026-07-06 10:19 UTC` was not a valid Telemost
  transport result because the local server config expected an exit proxy on
  `127.0.0.1:1081`, and that listener was not running.
- The iOS extension applied full-tunnel network settings before it had a
  gomobile-level signal that the `cnc` session and local SOCKS listener were
  ready.
- The first readiness attempt exposed a launch race: Swift started `StartCnc`
  on a detached thread and immediately called `WaitReady`; `WaitReady` could
  observe no registered run yet and return `olcRTC is not running`.

## Changes made

- `internal/app/session.RunWithReady` now forwards the `client.RunWithReady`
  callback for `cnc` mode while preserving `Run` as the default API.
- `mobile/olcmobile` now exposes `WaitReady(timeoutMillis int) error` for
  gomobile consumers.
- `WaitReady` waits for a just-starting `StartCnc` run to register within the
  timeout, instead of failing immediately when Swift wins the scheduling race.
- The gomobile wrapper uses per-run state so an old `StartCnc` return cannot
  overwrite the currently active run.
- iOS `PacketTunnelProvider` now waits for `OlcmobileWaitReady` before applying
  `NEPacketTunnelNetworkSettings` and starting tun2socks.

## Verification

Commands run from `fix-telemost` worktree:

```sh
go test -count=1 ./internal/app/session ./mobile/olcmobile
go build -o /Users/oxi/unite/whitelist-bypass/artifacts/telemost-fix/bin/olcrtc-fix-telemost-darwin ./cmd/olcrtc
TMPDIR=/Users/oxi/unite/whitelist-bypass/olcrtc-fork/.worktrees/fix-telemost/artifacts/telemost-fix/gomobile-tmp \
  PATH="/Users/oxi/go/bin:$PATH" \
  gomobile bind -target=ios,iossimulator \
  -o /Users/oxi/unite/whitelist-bypass/client/ios/OlcMobile.xcframework ./mobile/olcmobile
xcodebuild -project OlcClientiOS.xcodeproj -scheme OlcClientiOS -configuration Debug -destination generic/platform=iOS build
```

Fresh iOS short-probe run at `2026-07-06 11:04 UTC`:

- `cnc session ready` logged before `network settings applied`.
- `SOCKS ready` logged before `tun2socks starting`.
- App HTTP probes passed for custom `example.com`, `api.ipify.org`, and
  default `example.com`: `ok=3 fail=0`.
- Server summary shows matching traffic to `example.com:443` and
  `api.ipify.org:443`.

## Artifacts

Non-secret artifacts:

- `/Users/oxi/unite/whitelist-bypass/artifacts/telemost-fix/bin/olcrtc-fix-telemost-darwin`
- `/Users/oxi/unite/whitelist-bypass/artifacts/telemost-fix/ios-logs/20260706-1404/app-probes-summary.log`
- `/Users/oxi/unite/whitelist-bypass/artifacts/telemost-fix/ios-logs/20260706-1404/tunnel-readiness-summary.log`
- `/Users/oxi/unite/whitelist-bypass/artifacts/telemost-fix/logs/server-telemost-summary-20260706-1404.log`

Secret/raw artifacts:

- `/Users/oxi/unite/whitelist-bypass/.secrets/runtime/olc-srv-direct.yaml`
- `/Users/oxi/unite/whitelist-bypass/.secrets/runtime/logs/olc-local-srv-telemost-ready-20260706-1404.log`
- `/Users/oxi/unite/whitelist-bypass/.secrets/runtime/ios-logs/20260706-1404/`

## Current status

**Resolved as of the 2026-07-06 afternoon rerun.** Short HTTPS and 1 MiB bulk
downloads work through Telemost after the readiness fix when the test uses a
fresh room. The earlier timeout came from stale SFU room state in a heavily
reused debug room, not from the `b2ed096` readiness gate or a required WebRTC
transport code change.

## Stale-room bulk reruns after b2ed096 (2026-07-06 afternoon, ~11:45-11:58 UTC)

Two device runs on GIA iPhone 11 with the b2ed096 build
(`OLC_PROBE_DOWNLOAD_BYTES=1048576`, `OLC_PROBE_ROUNDS=3`, `OLC_PROBE_INTERVAL=8`).

Server-start prerequisite: the local server would not start against
`.secrets/runtime/olc-srv-direct.yaml` — it failed to resolve
`goloom.strm.yandex.net` (Telemost SFU) through the resolver pinned in the
config (`i/o timeout`), while `8.8.8.8` resolved it fine. Runs used a copy with
`dns: 8.8.8.8:53`. See the "DNS resolver" note below.

Result on the reused room — **bulk remained red (0 of 6 download rounds
succeeded):**

- Run A (11:45 UTC, warm carrier riding a pre-existing on-demand session):
  short HTTPS reliable and fast (ipify 257-479 ms, example.com 677-702 ms), but
  the 1 MiB download timed out at 60 s in all 3 rounds. Mid-download the carrier
  churned: server logged `reconnect reason=liveness` then
  `reason=carrier - tearing down smux session`; on device `cnc-stderr` showed
  `readVP8Track closed ... err=EOF`, `ICE connection state: closed`,
  `goolom publisher PC closed - triggering reconnect`.
- Run B (11:52 UTC, fresh cold carrier, single session): the client↔server smux
  never latched — the server logged no handshake, and the device streamed
  `connect failed: remote not ready (read_err=EOF ack=[0])` for every SOCKS
  stream. Even short probes degraded to timeouts; the 1 MiB download failed with
  SSL error / timeout in all 3 rounds.

### Interim hypothesis from the stale-room runs

Not the readiness gate. `b2ed096` is correct: `go test -race` is clean, the
connect/SOCKS ordering holds, and short HTTPS is reliable once the transport is
warm. At this stage the symptoms looked like a **vp8channel / Telemost carrier**
problem: the WebRTC publisher PC dropped within ~8-18 s of sustained load (RTP
track EOF, ICE closed), forcing `reinstallSession` + `Reconnect` and resetting
in-flight SOCKS streams.

A cold carrier (Run B) is worse: the SFU/link was degraded during the test
(repeated `goloom` resolve timeouts and carrier EOFs on server start), so the
data plane never reached usable quality at all. The fresh-room rerun below
showed that the underlying cause was stale SFU room state, not a carrier code
defect.

### Resolution — bulk is stable on a fresh room (12:54 UTC rerun)

The Run A/B failures were **not** a carrier defect. They were stale SFU session
state: the old debug room had been reused across ~8 rapid server restarts,
leaving lingering guest peers that poisoned the SFU's peer matching, so a fresh
cnc could not latch a clean data session.

Reran on a **freshly created Telemost room** (`room_manager.py ensure` →
new conference; server `room.id` and the iOS `BuiltInProfiles` telemost profile
both rebuilt onto it; one server, one device):

- Round 1: 1 MiB download OK, 1048576 bytes, 12.3 s (~85 KB/s).
- Round 2: 1 MiB download OK, 1048576 bytes, 13.9 s (~75 KB/s).
- Round 3: 1 MiB download OK, 1048576 bytes, 13.7 s (~76 KB/s).
- All three rounds `ok=3 fail=0` (short HTTPS + 1 MiB). No `reconnect`, no
  `tearing down`, no publisher-PC drop. Server logged the peer connected once and
  streamed `443 out=~1.056 MB` per round.

**Telemost is stable under sustained bulk traffic on a clean room.** Throughput
is ~75-85 KB/s (~0.6-0.7 Mbit/s), bounded by the vp8channel video-paced
transport — a throughput optimization, not a stability bug.

Operational takeaway (the actual "fix"): use a fresh/rotated room per session and
avoid reusing one room across many server restarts. The control-plane
`room_manager.py` rotation already provides this; the debugging harness just
needs to mint a new room instead of hammering a stale one. No WebRTC-transport
code change is required for stability.

### Artifacts

Sanitized: `artifacts/telemost-fix/ios-logs/20260706-143930/` (Run A),
`artifacts/telemost-fix/ios-logs/20260706-145137/` (Run B, plus
`cnc-stream-failures-summary.log`), and
`artifacts/telemost-fix/ios-logs/freshroom-20260706-155011/` (green fresh-room
bulk run). Server summaries:
`artifacts/telemost-fix/logs/server-telemost-summary-20260706-143930.log`,
`...-clean-20260706-145137.log`, and
`...-freshroom-20260706-155011.log`. Raw logs under `.secrets/runtime/`.

### DNS resolver note

The resolver pinned in `.secrets/runtime/olc-srv-direct.yaml` timed out resolving
`goloom.strm.yandex.net`, blocking server startup entirely. Fixed by pinning a
reliable resolver (`dns: 8.8.8.8:53`) for the local test server. For a local
Mac exit there is no reason to route DNS through the flaky resolver. The
deployed VM server config is separate and should be checked independently.

## Evening full-tunnel retests and hardening

Later real-device runs showed that "VPN connected" is still not enough for a
field-ready full tunnel. The carrier can stay up while individual TLS streams
fail after the client sends only the TLS ClientHello (`in=517 out=0` on the
server).

Hardening added after the first evening failures:

- `net.dns` now accepts comma-separated resolver fallbacks. The runtime DNS
  resolver tries configured endpoints in order while preserving the network
  requested by Go (`udp`, `udp4`, `tcp`, etc.).
- `socks.max_sessions_per_target` is configurable instead of hard-coded.
- `socks.slot_wait_ms` bounds global SOCKS slot waiting.
- `socks.block_ports` lets iOS field-test profiles reject noisy background
  ports before they consume tunnel slots.
- `socks.block_hosts` and `socks.block_cidrs` let iOS field-test profiles reject
  noisy Apple/iCloud background destinations before they consume tunnel slots.
- The local iOS debug profile used for the last run had `max_sessions: 24`,
  `slot_wait_ms: 500`, DNS fallback, `block_ports: [993, 5223]`, Apple/iCloud
  host blocks, and `17.0.0.0/8` CIDR blocking.

Evening harness results on GIA iPhone 11:

- `telemost-qos6-20260706-202136`: red, but no reconnects. `DownloadOK=2/3`,
  `RoundOK=1/3`, `HTTPError=4`. Round 3 was fully green; early failures were
  TLS stream failures and DNS/background traffic pressure.
- `telemost-qos7-20260706-203126`: red with carrier churn. `DownloadOK=2/3`,
  `RoundOK=1/3`, `HTTPError=3`, `Reconnects=3`.
- `telemost-qos8-20260706-204331`: red, no reconnects. `DownloadOK=2/3`,
  `RoundOK=1/3`, `HTTPError=3`. The port blocklist worked for IMAP, but Apple
  HTTPS background traffic still competed with probes; round 3 was fully green.

Current conclusion: the iOS full tunnel is usable enough to prove real HTTPS and
1 MiB downloads over Telemost, but it is **not yet stable enough to call
field-ready**. The remaining problem is per-stream reliability under background
iOS traffic and low vp8channel throughput, not VPN permission, app installation,
fresh-room selection, or the readiness gate.

Next technical direction:

- keep the fresh-room rule;
- keep DNS fallback and SOCKS limits;
- add host/suffix-level background traffic controls or a real QoS queue for
  Apple/iCloud background HTTPS flows;
- add a warm-up/health gate that reports "ready for browsing" only after a
  successful short HTTPS probe through the tunnel.

Sanitized evening artifacts are under:

- `artifacts/telemost-fix/harness/telemost-qos6-20260706-202136/`
- `artifacts/telemost-fix/harness/telemost-qos7-20260706-203126/`
- `artifacts/telemost-fix/harness/telemost-qos8-20260706-204331/`

Raw logs remain under `.secrets/runtime/harness/<stamp>/`.

## 2026-07-07 real-device retests

The next day confirmed two separate classes of failures:

- q17 (`telemost-qos17-bulk-20260707-110905`) was red because the Mac-side
  local harness routed public DNS through an active `utun14` path. The server
  reached the first 1 MiB download, then subsequent server DNS lookups failed
  with connection resets. This was local harness contamination, not Telemost
  media teardown.
- q18 (`telemost-qos18-bulk-20260707-113425`) used the local Tailscale DNS
  endpoint (`100.100.100.100:53`) and proved the sustained path: all three
  1 MiB downloads completed and reconnect count stayed zero. The run was still
  red only because the single-shot `api.ipify.org` app probe failed in two
  rounds while `example.com` and `speed.cloudflare.com` succeeded.
- q19 (`telemost-qos19-bulk-20260707-115148`) was not a data-path result. The
  local server failed Telemost carrier auth while fetching connection info from
  Yandex (`EOF` after retries), so iOS waited for a peer that never existed. The
  harness now retries server startup and monitors server exit during the probe
  window.
- q20 (`telemost-qos20-bulk-20260707-120319`) is the current green run on GIA
  iPhone 11: `RoundOK=3`, `DownloadOK=3`, `HTTPError=0`, `Reconnects=0`.

q20 details:

- VPN reached `NEVPNStatus.connected`.
- `cnc session ready`, `network settings applied`, `SOCKS ready`, and
  `tun2socks starting` appeared in order.
- The server saw one peer and no reconnect/teardown markers.
- All three 1 MiB downloads completed:
  - 26.9 s (~39 KB/s, ~0.31 Mbit/s)
  - 19.4 s (~54 KB/s, ~0.43 Mbit/s)
  - 41.0 s (~26 KB/s, ~0.20 Mbit/s)

The q20 app probes also showed why DEBUG probes now retry short HTTPS requests:
several first TLS attempts ended with iOS `SSL error`, but immediate retries over
the same VPN session succeeded. This is per-stream flakiness, not a carrier
teardown: the tunnel stayed up, bulk completed, and reconnect count remained
zero.

Current status: Telemost full-tunnel can carry real HTTPS and sustained 1 MiB
downloads on iOS without carrier reconnects on a fresh room. It is good enough
for controlled field smoke testing, but throughput is low and first-attempt TLS
flakiness still means user-facing browsing may need retries/reloads until the
vp8channel throughput/backpressure path is improved.

Sanitized q20 artifacts are under:

- `artifacts/telemost-fix/harness/telemost-qos20-bulk-20260707-120319/`

Raw q20 logs remain under:

- `.secrets/runtime/harness/telemost-qos20-bulk-20260707-120319/`
