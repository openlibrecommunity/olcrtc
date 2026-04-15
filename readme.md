<div align="center">

<img src="https://github.com/openlibrecommunity/material/blob/master/olcrtc.png" width="250" height="250">

![License](https://img.shields.io/badge/license-WTFPL-0D1117?style=flat-square&logo=open-source-initiative&logoColor=green&labelColor=0D1117)
![Golang](https://img.shields.io/badge/-Golang-0D1117?style=flat-square&logo=go&logoColor=00A7D0)

</div>

# Yandex Deleted DataChannel, project rewite to VideoChannel and Jazz , [wait and see](https://github.com/openlibrecommunity/olcrtc/issues/1) 

## About
olcRTC - across the Sea

Project that allows users to bypass blocking by parasitizing and tunneling on unblocked and whitelisted services in Russia, use telemost, Max, mail and API in the future

## jazz-visual status

`jazz-visual` is now working end-to-end through SaluteJazz SFU.

Current verified baseline:
- automated smoke / soak harness: [script/jazz_visual_smoke.sh](script/jazz_visual_smoke.sh)
- short soak: `ATTEMPTS=50` -> `50/50`
- longer soak: `ATTEMPTS=200` -> `200/200`
- live outer-FEC counters are non-zero during long runs, so recovery is exercised in practice

Native build with real VP8 path:

```bash
go build -tags vpx -o build/olcrtc ./cmd/olcrtc
```

Quick automated verification:

```bash
ATTEMPTS=10 script/jazz_visual_smoke.sh
```

The harness does the full cycle itself:
- kills old `srv/cnc`
- starts fresh `jazz-visual` server and client
- extracts a fresh `room:pass`
- waits for SOCKS5 readiness
- warms the tunnel with a preflight request
- runs counted `curl` checks through SOCKS5

Visual frame dumping is available for debugging:

```bash
./build/olcrtc -mode srv -provider jazz-visual -id any -key "$KEY" -visual-dump ./debug -debug
```

This writes every 30th visual frame as PNG:
- outbound: `out-000001.png`
- inbound: `in-000001.png`

## status

pre-alpha
<br>
see all info in [issues](https://github.com/openlibrecommunity/olcrtc/issues)
<br>
issues? contact us at [@openlibrecommunity](https://t.me/openlibrecommunity)
<br>
or wait for the release or at least a beta


## magefile

```bash
# install mage first
go install github.com/magefile/mage@latest

# build cli + ui
mage build

# build cli only
mage buildCLI

# build ui only
mage buildUI

# cross-compile for linux / windows / darwin
mage cross

# android aar via gomobile
mage mobile

# container image
mage podman
mage docker

# lint / test / clean
mage lint
mage test
mage clean
```


## fast start

```bash
# server ( podman, pre configured, easy, unix )
./script/srv.sh

# client ( podman, pre configured, easy, unix )  
./script/cnc.sh

# server ( podman, pre configured, easy, win )
./script/srv.bat

# client ( podman, pre configured, easy, win )  
./script/cnc.bat


# also there's a client UI version (currently in beta)
./script/ui.sh

# and then
./build/olcrtc-ui


# or native ( no podman ) cli linux
GOOS=linux GOARCH=amd64 go build ./cmd/olcrtc

# or native ( no podman ) cli android
GOOS=android GOARCH=arm64 go build -ldflags="-checklinkname=0" -o build/olcrtc ./cmd/olcrtc

# or native ( no podman ) cli windows
GOOS=windows GOARCH=amd64 go build ./cmd/olcrtc

# or native ( no podman ) ui linux
cd ui && go build -o ../build/olcrtc-ui .

# or native ( no podman ) ui windows
cd ui && GOOS=windows GOARCH=amd64 CGO_ENABLED=1 CC=x86_64-w64-mingw32-gcc go build -o ../build/olcrtc-ui.exe .


```

<div align="center">

---


Telegram: [zarazaex](https://t.me/zarazaexe)
<br>
Email: [zarazaex@tuta.io](mailto:zarazaex@tuta.io)
<br>
Site: [zarazaex.xyz](https://zarazaex.xyz)
<br>
Made for: [olcNG](https://github.com/zarazaex69/olcng)


</div>
