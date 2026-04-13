<div align="center">

<img src="https://github.com/openlibrecommunity/material/blob/master/olcrtc.png" width="250" height="250">

![License](https://img.shields.io/badge/license-WTFPL-0D1117?style=flat-square&logo=open-source-initiative&logoColor=green&labelColor=0D1117)
![Golang](https://img.shields.io/badge/-Golang-0D1117?style=flat-square&logo=go&logoColor=00A7D0)

</div>

# ЯНДЕКС УДАЛИЛ DataChannel !
# ПРОЕКТ ПЕРЕПИСЫВАЕТСЯ НА VideoChannel и SALUTEJAZZ, [ОЖИДАЙТЕ](https://github.com/openlibrecommunity/olcrtc/issues/1) !

## About
olcRTC - across the Sea

Project that allows users to bypass blocking by parasitizing and tunneling on unblocked and whitelisted services in Russia, use telemost, Max, mail and API in the future

## satus

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