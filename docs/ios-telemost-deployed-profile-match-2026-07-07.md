# iOS Telemost deployed profile match - 2026-07-07

## Scope

This note records the deployed-server iOS Telemost checks from 2026-07-07. The
main goal was to distinguish a real Telemost transport failure from an
operational config mismatch between the deployed server and the iOS app profile.

## Root cause

The first deployed-server attempt used a fresh Telemost room, but the iOS
profile was generated from stale deployment metadata while the server used a
different room/channel/key source.

Observed mismatch:

- iOS source shape: `channel_len=9`, `key_sha8=a30e04b2`.
- Server source shape: `channel_len=8`, `key_sha8=ef6cd81d`.
- Result: the client started data/control KCP and appeared to latch a peer, but
  SOCKS did not become usable; the app probe failed with
  `read welcome: timeout`.

Important operational detail: `push_room.sh` updates only the Telemost room id.
It does not rewrite the Telemost channel/key. A fresh room is not sufficient if
iOS and server are built from different channel/key sources.

## Corrected Telemost transport run

The corrected iOS build used server-matched room/channel/key for both sides.

- Stamp: `ios-telemost-fresh-match-20260707-180434`.
- Telemost profile shape: `room_len=43`, `channel_len=8`,
  `key_sha8=ef6cd81d`.
- Server saw one peer control session and one peer data session.
- iOS reached `NEVPNStatus.connected`, `cnc session ready`, and `SOCKS ready`.
- Probe result: `RoundOK=3`, `IpifyOK=3`, `ExampleOK=3`, `DownloadOK=3`.
- 1 MiB download durations: 7.6 s, 7.1 s, 9.0 s.
- No `read welcome timeout`, no `handshake failed`, no `bad header`, no
  publisher close, no server reconnect/teardown markers.

Sanitized artifacts:

- `artifacts/telemost-fix/harness/ios-telemost-fresh-match-20260707-180434/`

Raw configs, logs, and the built app with embedded profiles:

- `.secrets/runtime/harness/ios-telemost-fresh-match-20260707-180434/`

## App-level Telemost run

After the transport run, the same deployed Telemost setup was tested against
real iOS apps.

- Stamp: `ios-telemost-apps2-20260707-182659`.
- Combined verdict: `Green=true`.
- Public leak check: 0 lines.

Telegram:

- 102 Telegram connects.
- 94 Telegram traffic lines.
- 21 control-alive lines.
- 0 error-like lines.
- AFT generic text was delivered but did not trigger a bot reply.
- AFT `/start` did trigger a bot reply: send OK, wait OK, timeout=false,
  1 inbound item.

Safari:

- `api.ipify.org` connect and traffic were observed.
- Extended Safari window had 126 HTTPS traffic lines.
- 24 control-alive lines.
- 0 error-like lines.

YouTube:

- 18 YouTube host connects.
- 5 GoogleVideo connects.
- 27 Google APIs connects.
- 38 YouTube traffic lines.
- 7 control-alive lines.
- 0 error-like lines.

Sanitized app-level artifacts:

- `artifacts/app-ios/harness/ios-telemost-apps2-20260707-182659/`

Raw app-level logs:

- `.secrets/runtime/harness/ios-telemost-apps2-20260707-182659/`

## Related WB app-level run

The WB profile was verified separately in:

- `artifacts/wb-ios/harness/ios-wb-policy-20260707-190937/`

Result:

- `Green=true`.
- Long-run window: 2290 seconds.
- Bulk: 3 of 3 1 MiB downloads.
- Telegram traffic: 3730 server target lines.
- Reconnect/liveness: 0.
- Public leak check: 0 lines.

## Current conclusion

Both Telemost and WB carry real iOS app traffic when iOS and server configs are
generated from matching room/channel/key sources. Telemost is usable for
controlled field smoke testing. Throughput is still limited by `vp8channel`, so
high-throughput browsing/video remains an optimization target rather than a
basic connectivity blocker.
