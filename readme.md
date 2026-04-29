<div align="center">

<img src="https://github.com/openlibrecommunity/material/blob/master/olcrtc.png" width="250" height="250">

![License](https://img.shields.io/badge/license-WTFPL-0D1117?style=flat-square&logo=open-source-initiative&logoColor=green&labelColor=0D1117)
![Golang](https://img.shields.io/badge/-Golang-0D1117?style=flat-square&logo=go&logoColor=00A7D0)

</div>

# Yandex Deleted DataChannel, project rewite to VideoChannel 

## About
olcRTC - across the Sea

Project that allows users to bypass blocking by parasitizing and tunneling on unblocked and whitelisted services in Russia, use telemost, Max, mail and API in the future

## satus

alpha
<br>
see all info in [issues](https://github.com/openlibrecommunity/olcrtc/issues)
<br>
issues? contact us at [@openlibrecommunity](https://t.me/openlibrecommunity)
<br>
or wait for the release or at least a beta


## build

```bash
# install mage first
go install github.com/magefile/mage@latest

# build cli + ui
mage build

# build cli only
mage buildCLI

# build cli with b codec, clones b repo, builds libb.so, compiles with -tags b
mage buildB

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
