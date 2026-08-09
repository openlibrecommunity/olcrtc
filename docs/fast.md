<div align="center">

<img src="https://github.com/openlibrecommunity/material/blob/master/olcrtc.png" width="250" height="250">

![License](https://img.shields.io/badge/license-WTFPL-0D1117?style=flat-square&logo=open-source-initiative&logoColor=green&labelColor=0D1117)
![Golang](https://img.shields.io/badge/-Golang-0D1117?style=flat-square&logo=go&logoColor=00A7D0)

[RU](fast.ru.md) / **EN**

</div>

# Quick start (via scripts)

> **Important:** always check that the video call service you need is on the allow lists, that it works in your network, and so on. If not, use another one.

This is the easiest way. Everything runs inside a [Podman](https://en.wikipedia.org/wiki/Podman) container. The script does everything itself: clones the [sources](https://github.com/openlibrecommunity/olcrtc), builds the binary in a container, generates a config and starts it.

The project is in beta. For issues: t.me/openlibrecommunity

If you want to build the binary by hand, see [manual.md](manual.md).

---

## What to install

### git

```sh
apt install git        # Debian / Ubuntu / Mint
pacman -S git          # Arch / CachyOS / Manjaro
dnf install git        # Fedora / RHEL / CentOS
```

### curl

```sh
apt install curl       # Debian / Ubuntu / Mint
pacman -S curl         # Arch / CachyOS / Manjaro
dnf install curl       # Fedora / RHEL / CentOS
```

### podman

You do not have to install it up front - the script installs podman itself if it is missing. But if you want:

```sh
apt install podman     # Debian / Ubuntu / Mint
pacman -S podman       # Arch / CachyOS / Manjaro
dnf install podman     # Fedora / RHEL / CentOS
```

### swap (RAM)

If the machine has less than 4 GB RAM, the build may crash. **Enable SWAP**:

```sh
sudo fallocate -l 4G /swapfile && sudo chmod 600 /swapfile && sudo mkswap /swapfile && sudo swapon /swapfile
```

---

## Step 1: Clone the repository

```sh
git clone https://github.com/openlibrecommunity/olcrtc --recurse-submodules
cd olcrtc
```

<img src="asset/gitclone.png" alt="git clone" width="800">

---

## Step 2: Run the server

On the machine the traffic should go through (VPS, server abroad, home PC):

```sh
./script/srv.sh
```

The script installs podman if needed, clones the current code, builds the binary in a container and starts the server. Then it asks a few questions.

#### `srv.sh` flags

| Flag | What it does |
|---|---|
| `--branch=<name>` | Use another repository branch instead of `master` |
| `--no-cache` | Purge the Go cache (`~/.cache/olcrtc`) before building - rebuild from scratch |

```sh
./script/srv.sh --no-cache               # build from scratch
./script/srv.sh --branch=dev --no-cache  # branch dev, no cache
```

### Carrier (which service carries the traffic)

```
Select carrier:
  1) jitsi
  2) telemost
  3) wbstream
Enter choice [1-3, default: 1]:
```

**Default is `jitsi`** - stable on datachannel, no registration needed, easy to self-host. Full compatibility matrix in [settings.md](settings.md).

### Transport (how the data is carried)

```
Select transport:
  1) datachannel
  2) videochannel
  3) seichannel
  4) vp8channel
Enter choice [1-4, default: 1]:
```

Recommendations:
- **datachannel** - fastest, lowest ping. Stable with `jitsi`. **WBStream DC does not work** in the normal guest flow. **Telemost removed DC**.
- **vp8channel** - works with telemost and wbstream, fast, but high ping.
- **seichannel** - works only with wbstream, slow, but low ping.
- **videochannel** - works with wbstream reliably, with telemost when possible; slowest and highest ping.

**Recommended combo: `jitsi + datachannel`**. Alternative: `wbstream + vp8channel`.

### Jitsi server (carrier jitsi only)

```
Enter a Jitsi room URL (https://HOST/ROOM).
Pick a HOST from docs/examples/jitsi.instances.yaml and verify it opens in a browser.
```

Pick the one that **opens in your browser**. Any public or self-hosted Jitsi Meet works.

### Room (carrier jitsi only)

```
Room options:
  1) Auto-generate new room (recommended)
  2) Use specific room name or URL
Enter choice [1-2, default: 1]:
```

- **1) Auto-generate** - the script picks a room name on the chosen server. Recommended.
- **2) Specific** - enter a room name (`myroom`) or a full URL (`https://meet.example.org/myroom`).

For **telemost** and **wbstream** the Jitsi menu is not shown - the script asks for the Room ID directly. Create a room on the site ([telemost](https://telemost.yandex.ru/), [wbstream](https://stream.wb.ru)) and paste its ID.

### DNS

```
DNS server [default: 8.8.8.8:53]:
```

Press Enter. No need to change it without a reason; if you want - `77.88.8.8:53` or your provider's DNS.

### SOCKS5 proxy for egress

```
Use SOCKS5 proxy for egress? (y/N):
```

If not needed - just Enter. If the server itself should go through an external proxy - type `y` and enter the address and port.

### Transport settings (videochannel only)

```
Video codec:
  1) qrcode
  2) tile (requires 1080x1080)
Enter choice [1-2, default: 1]:
```

- **qrcode** - QR codes, configurable resolution, stable, slow.
- **tile** - tile codec, 1080x1080 only, Reed-Solomon support, faster but less stable.

Then the script asks for width/height, QR error correction, fragment size (or tile params), FPS, bitrate and hardware acceleration (`none`/`nvenc`). Press Enter for defaults.

### Transport settings (vp8channel only)

```
VP8 FPS [default: 25]:
VP8 batch size (frames per tick) [default: 1]:
```

Press Enter if `25` and `1` are fine.

### Transport settings (seichannel only)

```
SEI FPS [default: 60]:
SEI batch size (frames per tick) [default: 64]:
SEI fragment size in bytes [default: 900]:
SEI ACK timeout in milliseconds [default: 2000]:
```

Press Enter on all - the defaults are optimal.

### Config comment

```
Enter a comment for the config (default: olc - t.me/openlibrecommunity):
```

This is the label for the resulting `olcrtc://` URI. You can leave it empty (Enter).

### Result

After startup the script prints the container name, carrier, transport, Room ID, the **encryption key** and a ready `olcrtc://` URI:

```
[+] Server started successfully!

Container name: olcrtc-server-xxxxxxxx
Carrier:        jitsi
Transport:      datachannel
Room ID/URL:    https://meet.example.org/olcrtc-xxxxxxxx
Encryption key: d823fa01cb3e0609b67322f7cf984c4ee2e294936fc24ef38c9e59f4799...

uri: olcrtc://jitsi?datachannel@https://meet.example.org/olcrtc-xxxxxxxx#<key>$olc - t.me/openlibrecommunity
```

**Save the Room ID and the encryption key** - the client needs them. The key is also saved to `~/.olcrtc_key` and reused on later runs.

---

## Step 3: Run the client

On your machine (home PC, laptop):

> Prefer a ready-made Android client? Use [owenewans/owenclave](https://github.com/owenewans/owenclave) ([src.owenewans.org/owenrtc](https://src.owenewans.org/owenrtc)) - it reads the `olcrtc://` URI and subscriptions directly, no binary needed. The steps below run the native `cnc` binary (SOCKS5 only).

```sh
git clone https://github.com/openlibrecommunity/olcrtc --recurse-submodules
cd olcrtc
./script/cnc.sh
```

Answer the same questions as on the server - **auth, transport and room ID must match**. For jitsi the script asks for the same server choice and room name/URL.

When it asks for the key:

```
Enter Encryption Key (hex):
```

Paste the key from the server (64 hex characters).

### SOCKS5: address, port, auth

```
SOCKS5 ip [default: 127.0.0.1]:
SOCKS5 port [default: 8808]:
SOCKS5 username (leave empty to disable auth):
```

Press Enter for address and port - the proxy comes up on `127.0.0.1:8808`. If you want login/password protection - enter the username, then the password. When binding outside loopback (not `127.*`) a username and password are required.

### Result

```
[+] Client started successfully!

Container name: olcrtc-client-xxxxxxxx
Auth:           jitsi
Transport:      datachannel
Room ID/URL:    https://meet.example.org/olcrtc-xxxxxxxx
SOCKS5 proxy:   127.0.0.1:8808
```

---

## Step 4: Verify

```sh
curl --socks5-hostname 127.0.0.1:8808 https://icanhazip.com
```

It should return your server IP.

Or route all traffic through the proxy:

```sh
export all_proxy=socks5h://127.0.0.1:8808
curl https://icanhazip.com
```

---

## Control

### Logs

```sh
podman ps --filter name=olcrtc
podman logs -f olcrtc-server-xxxxxxxx   # on the server
podman logs -f olcrtc-client-xxxxxxxx   # on the client
```

### Stop

```sh
podman stop olcrtc-server-xxxxxxxx
podman stop olcrtc-client-xxxxxxxx
```

Stop all olcrtc containers at once:

```sh
podman stop $(podman ps -q --filter name=olcrtc)
```

---

## Update a running instance

A running container does not update itself: it keeps the binary built at start time. To move to the current code, stop the old container and run the script again.

```sh
cd olcrtc
git pull --recurse-submodules          # update local scripts
podman stop olcrtc-server-xxxxxxxx     # stop the old container
./script/srv.sh --no-cache             # start again with fresh code
```

`--no-cache` is optional but guarantees a clean rebuild. On the rerun use the same `auth`, `transport`, `room ID` and key. The server key lives in `~/.olcrtc_key` and is reused automatically.

---

## Multiple instances on one machine

You can run several servers or clients on one machine - each run creates a container with a unique name (`olcrtc-server-<random>`), they do not conflict.

```sh
./script/srv.sh   # first instance - e.g. jitsi + datachannel
./script/srv.sh   # second instance - e.g. wbstream + vp8channel
```

On the client run a separate `cnc.sh` for each instance with **different SOCKS5 ports** to switch between them:

```sh
./script/cnc.sh   # first client - port 8808 (default)
./script/cnc.sh   # second client - set port 8809
```

---

All settings and the compatibility matrix: [settings.md](settings.md). Manual build without scripts: [manual.md](manual.md).
