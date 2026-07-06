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

## Remaining problem

Short HTTPS works through Telemost after the readiness fix. The 1 MiB
Cloudflare download test still timed out in the earlier `2026-07-06 10:47 UTC`
run, before the `WaitReady` launch-race fix was applied. A fresh bulk/stability
run is still needed before claiming Telemost is stable under sustained traffic.
