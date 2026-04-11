<div align="center">

<img src="https://github.com/openlibrecommunity/material/blob/master/olcrtc.png" width="250" height="250">

![License](https://img.shields.io/badge/license-WTFPL-0D1117?style=flat-square&logo=open-source-initiative&logoColor=green&labelColor=0D1117)
![Golang](https://img.shields.io/badge/-Golang-0D1117?style=flat-square&logo=go&logoColor=00A7D0)

</div>


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


## fast start

```bash
# server ( podman, pre configured, easy )
./script/srv.sh

# client ( podman, pre configured, easy   
./script/cnc.sh


# Also there's a client UI version (currently in beta)
./script/ui.sh

# And then
./build/olcrtc-ui


# or native ( no podman ) linux
GOOS=linux GOARCH=amd64 go build ./cmd/olcrtc

# or native ( no podman ) android
GOOS=android GOARCH=arm64 go build -ldflags="-checklinkname=0" -o build/olcrtc ./cmd/olcrtc

# or native ( no podman ) windows
GOOS=windows GOARCH=amd64 go build ./cmd/olcrtc


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