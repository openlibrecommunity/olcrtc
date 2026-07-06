# iOS Telemost harness

`ios-telemost-harness` automates the local Telemost iOS stability test that was
previously run by hand.

## What it does

- Creates a fresh Telemost room for each run by using a run-specific
  `rooms.json` store, so stale SFU peers from previous server restarts cannot
  poison the test.
- Runs a DNS preflight for `goloom.strm.yandex.net` through the configured
  resolver, then pins the same resolver into the local srv YAML.
- Renders a local srv config and iOS `BuiltInProfiles.local.json` from the same
  subscription.
- Optionally builds, installs, launches, and probes the iOS app through
  `devicectl`.
- Copies raw iOS/server logs under `.secrets/runtime/harness/<stamp>/`.
- Writes sanitized summaries and `verdict.json` under
  `artifacts/telemost-fix/harness/<stamp>/`.

## Commands

Dry-run the full path/command plan:

```sh
go run ./cmd/ios-telemost-harness run \
  --workspace /Users/oxi/unite/whitelist-bypass \
  --stamp dryrun-verify \
  --dry-run
```

Prepare a fresh room and local configs without running iOS:

```sh
go run ./cmd/ios-telemost-harness prepare \
  --workspace /Users/oxi/unite/whitelist-bypass \
  --stamp telemost-$(date +%Y%m%d-%H%M%S)
```

Run the full iOS probe:

```sh
go run ./cmd/ios-telemost-harness run \
  --workspace /Users/oxi/unite/whitelist-bypass \
  --device 406AE25C-CC1F-592D-A60B-872C2D2E6427 \
  --stamp telemost-$(date +%Y%m%d-%H%M%S) \
  --rounds 3 \
  --download-bytes 1048576 \
  --probe-interval 8 \
  --wait-seconds 240
```

Rebuild summaries/verdict from an existing harness run:

```sh
go run ./cmd/ios-telemost-harness summarize \
  --workspace /Users/oxi/unite/whitelist-bypass \
  --stamp <existing-stamp> \
  --rounds 3 \
  --download-bytes 1048576
```

## Defaults

- Cookies: `.secrets/telemost-account/cookie-header.txt`
- Deployment: `.secrets/olc-stand/deployment.json`
- Server template: `.secrets/runtime/olc-srv-direct.yaml`
- Resolver: `8.8.8.8:53`
- Rounds: `3`
- Download bytes: `1048576`
- Probe interval: `8`

## Verdict

Green requires:

- every probe round to finish with `fail=0`;
- one successful `label=download` probe per expected round when
  `--download-bytes > 0`;
- zero app-level HTTP probe errors;
- zero server/tunnel reconnect or teardown markers in the parsed logs.

Red writes the concrete reasons into `verdict.json`.

## Live result on 2026-07-06

The first real device run exposed two harness problems that are now fixed:

- Framework Python on this Mac did not have a default CA file, so room creation
  failed with `CERTIFICATE_VERIFY_FAILED`. The harness now injects the local
  `certifi` CA bundle for `room_manager.py` when no explicit SSL CA env is set.
- Xcode 17 stalled in the classic linker when the project-level
  `OTHER_LDFLAGS=-ld_classic -lresolv` was used. The harness build command
  overrides this to `OTHER_LDFLAGS=-lresolv`; the same app build completed and
  installed on the iPhone.

The clean fresh-room iPhone run still ended red as a product result, not as a
harness/build failure. The VPN connected and the server saw one peer. Two 1 MiB
Cloudflare downloads completed, but later probe sessions failed with SSL errors
while the server logged `in=0 out=0` for those probe TCP sessions. The raw log
also showed bursts of background iOS Mail/iCloud/Apple TCP sessions at the same
time. Current conclusion: the carrier stayed up, but the narrow vp8channel is
not yet robust under real full-tunnel background traffic. The next fix should
target tunnel-side backpressure/QoS/session limits, then rerun this harness.

## Secret handling

Raw files can contain room IDs, keys, and local configs. They stay under:

```text
/Users/oxi/unite/whitelist-bypass/.secrets/runtime/harness/<stamp>/
```

Only sanitized summaries are written under:

```text
/Users/oxi/unite/whitelist-bypass/artifacts/telemost-fix/harness/<stamp>/
```
