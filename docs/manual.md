<div align="center">

<img src="https://github.com/openlibrecommunity/material/blob/master/olcrtc.png" width="250" height="250">

![License](https://img.shields.io/badge/license-WTFPL-0D1117?style=flat-square&logo=open-source-initiative&logoColor=green&labelColor=0D1117)
![Golang](https://img.shields.io/badge/-Golang-0D1117?style=flat-square&logo=go&logoColor=00A7D0)

[RU](manual.ru.md) / **EN**

</div>

# Manual build

> **Important:** always check whether the video call service you need is on the allow lists. If it is not there, use another one. A list of all allow-listed services will be published soon.


This way is for those who want to build the native binary by hand.
You need Go 1.26+, mage, git.

---


### swap (RAM)

If you have less than 4 GB of RAM, the build may crash. **Be sure to enable SWAP**:

```bash
sudo fallocate -l 4G /swapfile && sudo chmod 600 /swapfile && sudo mkswap /swapfile && sudo swapon /swapfile
```


---

## What to install

## Step 1: Install git

```sh
apt install git       # Debian   / Ubuntu  / Mint
pacman -S git         # Arch    / CachyOS / Manjaro
dnf install git       # Fedora / RHEL   / CentOS
```

---

## Step 2: Install Go 1.26+

### Arch / Fedora (easy)

```sh
pacman -S go    # Arch    / CachyOS / Manjaro
dnf install go  # Fedora / RHEL   / CentOS
```

### Debian / Ubuntu (the system package is outdated)

On Debian/Ubuntu the repository usually has Go 1.19.

On Debian 13 it is better to go through `testing` with `APT Pinning`, so as not to pollute the OS:

```sh
echo 'deb http://deb.debian.org/debian/ testing main non-free-firmware' | sudo tee /etc/apt/sources.list.d/testing.list

cat <<EOF | sudo tee /etc/apt/preferences.d/testing-pin
Package: *
Pin: release a=testing
Pin-Priority: 100
EOF

sudo apt update
sudo apt install -t testing golang-go

sudo update-alternatives --install /usr/bin/go go `which go` 10
sudo update-alternatives --install /usr/bin/gofmt gofmt `which gofmt` 10
```

Otherwise via the SDK:

```sh
apt install golang                         # install the old go - it is only needed to download the new one
go install golang.org/dl/go1.26.0@latest   # download the go1.26 installer
~/go/bin/go1.26.0 download                 # download go1.26 itself
mv ~/go/bin/go1.26.0 /usr/local/bin/go     # replace the system go
```

### Check

```sh
go version
# go version go1.26.x linux/amd64
```

---

## Step 3: Install mage

mage is a build system for Go projects, similar to make.

```sh
go install github.com/magefile/mage@latest
```

Add `~/go/bin` to PATH:

```sh
echo 'export PATH="$HOME/go/bin:$PATH"' >> ~/.bashrc
source ~/.bashrc
```

Check:

```sh
mage --version
# mage vx.x.x
```

---

## Step 4: Clone the repository

```sh
git clone https://github.com/openlibrecommunity/olcrtc
cd olcrtc
```


---

## Step 5: Build

```sh
mage build   # current platform
mage cross   # all platforms at once (if you build for another machine)
```

The result is in `build/`:

```
build/olcrtc-linux-amd64
```

---

## Step 6: Generate the encryption key

Done once on the server. The key must match on server and client.

```sh
openssl rand -hex 32 
# d823fa01cb3e0609b67322f7cf984c4ee2e4ce2e294936fc24ef38c9e59f4799
```

Save the output - you will need it when running the client.

---

## Step 7: Run the server

On the server machine (VPS, etc.). Pick the right auth provider + transport combination from the matrix in [settings.md](settings.md).

### jitsi + datachannel (recommended)

The simplest way: use any self-hosted or public Jitsi Meet instance. No registration needed, the room name is made up on the fly. Available instances are listed in [`docs/examples/jitsi.instances.yaml`](./examples/jitsi.instances.yaml) - **be sure to check in a browser which one works in your network** and use the one that opens. Any other one will also do (`meet.jit.si`, your own self-hosted, etc.).

Create a YAML config:

```yaml
# server.yaml
mode: srv
auth:
  provider: jitsi
room:
  # Instances: see docs/examples/jitsi.instances.yaml
  id: "https://meet.example.org/myroom"
crypto:
  key: "d823fa01cb3e0609b67322f7cf984c4ee2e4ce2e294936fc24ef38c9e59f4799"
net:
  transport: datachannel
  dns: "8.8.8.8:53"
data: data
```

Run:

```sh
./build/olcrtc-linux-amd64 server.yaml
```

The server joins the room itself (as a participant without camera/microphone) and waits for the client to join too. Without a second participant Jicofo does not issue a session-initiate - that is a Jitsi quirk.

### wbstream + vp8channel (alternative)

Create a room through the [wbstream](https://stream.wb.ru) site and paste its ID into `room.id`.

`wbstream + datachannel` **does not work** in the normal guest flow - WB Stream issues tokens with `canPublishData=false`, and DC does not route data. For normal use pick `vp8channel`.

Create a YAML config:

```yaml
# server.yaml
mode: srv
auth:
  provider: wbstream
room:
  id: "<room-id-from-stream.wb.ru>"
crypto:
  key: "d823fa01cb3e0609b67322f7cf984c4ee2e4ce2e294936fc24ef38c9e59f4799"
net:
  transport: vp8channel
  dns: "8.8.8.8:53"
data: data
```

Run:

```sh
./build/olcrtc-linux-amd64 server.yaml
```

The Room ID must be passed to the client.

### Add debug

Add `debug: true` to the YAML config - you will see every connection:

```
2026/05/03 08:05:23 Connecting link via direct/vp8channel/wbstream...
2026/05/03 08:05:25 wbstream publisher state: connected
2026/05/03 08:05:27 Link connected
2026/05/03 08:05:43 sid=3 connect icanhazip.com:443
2026/05/03 08:05:43 sid=3 connected icanhazip.com
```

---

## Step 8: Run the client

On your machine. `auth.provider`, `net.transport`, `room.id` and `crypto.key` must match the server.

> Prefer a ready-made Android client instead of a YAML config? Use [owenewans/owenclave](https://github.com/owenewans/owenclave) ([src.owenewans.org/owenrtc](https://src.owenewans.org/owenrtc)) - it reads the `olcrtc://` URI and subscriptions directly. The steps below run the native `cnc` binary (SOCKS5 only).

### jitsi + datachannel (recommended)

```yaml
# client.yaml
mode: cnc
auth:
  provider: jitsi
room:
  # Instances: see docs/examples/jitsi.instances.yaml
  id: "https://meet.example.org/myroom"
crypto:
  key: "<hex-key-same-as-on-the-server>"
net:
  transport: datachannel
  dns: "8.8.8.8:53"
socks:
  host: "127.0.0.1"
  port: 8808
data: data
```

```sh
./build/olcrtc-linux-amd64 client.yaml
```

After it starts, SOCKS5 listens on `127.0.0.1:8808`. Use any client with SOCKS5 support (`curl --socks5 127.0.0.1:8808 ...`, a browser with a proxy switcher, etc.).

### wbstream + vp8channel (alternative)

```yaml
# client.yaml
mode: cnc
auth:
  provider: wbstream
room:
  id: "<room-id>"
crypto:
  key: "<hex-key>"
net:
  transport: vp8channel
  dns: "8.8.8.8:53"
socks:
  host: "127.0.0.1"
  port: 8808
data: data
```

```sh
./build/olcrtc-linux-amd64 client.yaml
```

After it starts the logs will show:

```
SOCKS5 server listening on 127.0.0.1:8808
```

If you need to protect the proxy with a login and password (for example on a machine with multiple users), add `socks.user` and `socks.pass` to the config:

```yaml
# client.yaml
mode: cnc
auth:
  provider: wbstream
room:
  id: "<room-id>"
crypto:
  key: "<hex-key>"
net:
  transport: vp8channel
  dns: "8.8.8.8:53"
socks:
  host: "127.0.0.1"
  port: 8808
  user: myuser
  pass: mypass
data: data
```

Without these fields authentication is disabled - the behavior is the same as before.

---

## Step 9: Check

```sh
curl --socks5-hostname 127.0.0.1:8808 https://icanhazip.com
```

It should return the server IP.


---

## Update the binary and an already running instance

A running process does not update itself: it keeps working with the old binary even if the repository is already updated. You need to fetch fresh code, rebuild the binary and restart the process or systemd service.

### 1. Update the repository

```sh
cd olcrtc
git pull --recurse-submodules
```

If you work not with `master`, switch to the needed branch first:

```sh
git switch dev
git pull --recurse-submodules
```

### 2. Rebuild the binary

```sh
mage build
```

For the current Linux machine the result is usually here:

```sh
build/olcrtc-linux-amd64
```

For another architecture see the file name in `build/` or build all platforms at once:

```sh
mage cross
ls build/
```

### 3. Stop the old process

If you started it manually in a terminal - stop it with `Ctrl+C`.

If the process runs in the background, find it:

```sh
pgrep -af olcrtc
```

And stop the right PID:

```sh
kill <pid>
```

If olcrtc runs through systemd, there is no need to stop it by hand - a service restart after updating the binary is enough.

### 4. Replace the binary if it lives outside `build/`

If you run straight from `build/`, this step is not needed.

If you copied the binary to a system path, update the copy:

```sh
sudo install -m 0755 build/olcrtc-linux-amd64 /usr/local/bin/olcrtc
```

The path and file name may differ if the machine is not `linux/amd64`.

### 5. Start again with the same config

Server:

```sh
./build/olcrtc-linux-amd64 server.yaml
```

Client:

```sh
./build/olcrtc-linux-amd64 client.yaml
```

If you use systemd:

```sh
sudo systemctl restart olcrtc-server
sudo systemctl restart olcrtc-client
```

Service names depend on how you created them. The configs do not need to change if `auth`, `transport`, `room ID`, the key and the SOCKS5 port stay the same.

---

## All mage targets

### Build
```sh
mage build    # build for the current platform
mage cross    # build for all platforms
mage mobile   # build the Android AAR
mage clean    # remove build/
```

### Quality
```sh
mage vet      # go vet
mage lint     # golangci-lint
mage tidy     # go mod tidy && go mod verify
mage deps     # go mod download
```

### Tests
```sh
mage test       # units in -short, fast
mage testFull   # all units + local e2e with -race
mage e2e        # smoke matrix against real providers
mage stress     # stress matrix (~6 h)
mage soak       # real soak (hours)
mage localSoak  # in-memory soak (no network)
```

### Pipelines
```sh
mage check       # build + vet + lint + testFull (before a commit)
mage all         # check + e2e (before merging a PR)
mage nightly     # all + stress (nightly CI, ~6 h)
mage everything  # nightly + soak + localSoak (full validation, 12+ h)
```

### Misc
```sh
mage help     # list targets in the standard mage style
mage -l       # same as mage help
mage         # no arguments = mage help
```

Fine-tune the test runs through environment variables:

```sh
# a single stress case
E2E_CARRIERS=telemost E2E_TRANSPORTS=videochannel \
    STRESS_BULK_DURATION=0 STRESS_ECHO_DURATION=0 \
    STRESS_CASE_TIMEOUT=2m STRESS_TIMEOUT=3m mage stress

# soak only jitsi for 30 minutes
SOAK_CARRIERS=jitsi SOAK_DURATION=30m mage soak
```

Full list of variables:
- `E2E_CARRIERS`, `E2E_TRANSPORTS`, `E2E_TIMEOUT`, `E2E_STRESS`, `E2E_STRESS_DURATION`
- `STRESS_BULK_DURATION`, `STRESS_ECHO_DURATION`, `STRESS_CASE_TIMEOUT`, `STRESS_TIMEOUT`
- `SOAK_CARRIERS`, `SOAK_TRANSPORTS`, `SOAK_DURATION`, `SOAK_CHAOS`

---

## Multiple instances on one server

You can run several olcrtc servers on one machine - each with its own config (different providers, rooms, transports). For this, create a separate YAML file for each instance and run each in its own process.

### Example: two servers

```yaml
# server-jitsi.yaml
mode: srv
auth:
  provider: jitsi
room:
  id: "https://meet.example.org/room1"
crypto:
  key: "aaaa...1111"
net:
  transport: datachannel
  dns: "8.8.8.8:53"
data: data
```

```yaml
# server-wbstream.yaml
mode: srv
auth:
  provider: wbstream
room:
  id: "<room-id>"
crypto:
  key: "bbbb...2222"
net:
  transport: vp8channel
  dns: "8.8.8.8:53"
data: data
```

Run each in its own terminal (or via `tmux` / `screen` / `systemd`):

```sh
./build/olcrtc-linux-amd64 server-jitsi.yaml
./build/olcrtc-linux-amd64 server-wbstream.yaml
```

### Clients

On the client machine - one config per server, with **different SOCKS5 ports**:

```yaml
# client-jitsi.yaml
mode: cnc
auth:
  provider: jitsi
room:
  id: "https://meet.example.org/room1"
crypto:
  key: "aaaa...1111"
net:
  transport: datachannel
  dns: "8.8.8.8:53"
socks:
  host: "127.0.0.1"
  port: 8808
data: data
```

```yaml
# client-wbstream.yaml
mode: cnc
auth:
  provider: wbstream
room:
  id: "<room-id>"
crypto:
  key: "bbbb...2222"
net:
  transport: vp8channel
  dns: "8.8.8.8:53"
socks:
  host: "127.0.0.1"
  port: 8809
data: data
```

```sh
./build/olcrtc-linux-amd64 client-jitsi.yaml      # SOCKS5 on :8808
./build/olcrtc-linux-amd64 client-wbstream.yaml    # SOCKS5 on :8809
```

Switching between instances in olcbox is just picking the right SOCKS5 port.

---

Need a short path without the details? -> [Quick start](fast.md)

All settings and the compatibility matrix -> [settings.md](settings.md)
