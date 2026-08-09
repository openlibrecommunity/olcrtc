<div align="center">

<img src="https://github.com/openlibrecommunity/material/blob/master/olcrtc.png" width="250" height="250">

![License](https://img.shields.io/badge/license-WTFPL-0D1117?style=flat-square&logo=open-source-initiative&logoColor=green&labelColor=0D1117)
![Golang](https://img.shields.io/badge/-Golang-0D1117?style=flat-square&logo=go&logoColor=00A7D0)

[RU](sub.ru.md) / **EN**

</div>

# Subscription format `sub.md`

`sub.md` is a plain text file hosted on a server and served as plain text.

Example URL:

```text
https://killpeople.freegore.xyz/sub
```

Inside, the file holds a list of `olcrtc` URIs from [uri.md](uri.md) plus extra technical fields for the client.

Important: this is a convention **for client applications**. `olcrtc` itself does not read or process such a file.

---

## Purpose

The format is meant for client subscriptions:

- a list of servers in one file
- subscription metadata for the UI
- metadata for individual servers
- info for auto-updating the subscription

---

## Overall structure

The file is read top to bottom and consists of:

1. global subscription fields prefixed with `#`
2. `olcrtc://...` lines
3. local fields of a specific server prefixed with `##`

Base schema:

```text
#name: ...
#update: ...
#refresh: ...
#color: ...
#icon: ...
#used: ...
#available: ...

olcrtc://...
##name: ...
##color: ...
##icon: ...
##used: ...
##available: ...
##ip: ...
##comment: ...

olcrtc://...
##name: ...
##comment: ...
```

---

## Global subscription fields

Lines like `#key: value` apply to the whole subscription.

| Field | Meaning |
|------|----------|
| `#name:` | Subscription name |
| `#update:` | Time of the last update in Unix time |
| `#refresh:` | How often the client should refresh the subscription, e.g. `5s`, `10m`, `6h` |
| `#color:` | Subscription color. UI-only field |
| `#icon:` | Subscription icon. UI-only field |
| `#used:` | How much is already used, e.g. `10mb/10gb` |
| `#available:` | How much is available in total under the subscription, e.g. `1.1gb` |

`#available:` is the value at the level of the whole subscription. If the client can count the remainder itself, it may use this field as source data or as a displayed hint.

---

## Server lines

Each server line holds one `olcrtc` URI in the format from [uri.md](uri.md):

```text
olcrtc://<Auth>?<Transport>@<RoomID>#<EncryptionKey>$<MIMO>
olcrtc://<Auth>?<Transport><key=value&key=value>@<RoomID>#<EncryptionKey>$<MIMO>
```

One line = one server / one subscription entry.

Empty lines between items are allowed.

---

## Local server fields

Lines like `##key: value` apply only to the **last URI** declared above.

That is, the client must bind a `##...` block to the nearest preceding `olcrtc://...` line.

| Field | Meaning |
|------|----------|
| `##name:` | Server/node name |
| `##color:` | UI color |
| `##icon:` | UI icon |
| `##used:` | Usage for a specific server, e.g. `500mb/10gb` |
| `##available:` | Available volume for a specific server |
| `##ip:` | Server IP address, if it needs to be shown to the client |
| `##comment:` | Free-form comment |

Local fields almost duplicate the global ones, but without `refresh`, because the update period is set at the whole-subscription level.

## Value recommendations

- For `#update:` use Unix time in seconds.
- For `#refresh:` use short intervals like `5s`, `10m`, `6h`, `1d`.
- For `#color:` use one stable format within the client, e.g. `#RRGGBB`.
- For `#icon:` use a string identifier or emoji.
- For `#used:` and `#available:` use human-readable units `kb`, `mb`, `gb`, `tb`.

---

## Full example

```text
#name: Zarazaex Free RU
#update: 1778011200
#refresh: 10m
#color: #4A90E2
#icon: 🇷🇺
#used: 10mb/10gb
#available: 9.99gb

olcrtc://wbstream?seichannel<fps=60&batch=64&frag=900&ack-ms=2000>@room-01#d823fa01cb3e0609b67322f7cf984c4ee2e4ce2e294936fc24ef38c9e59f4799$RU / olcng free sub / IPv6
##name: RU-1
##icon: 🇷🇺
##color: #4A90E2
##used: 500mb/10gb
##available: 9.5gb
##ip: 203.0.113.10
##comment: basic free node

olcrtc://wbstream?datachannel@abc123xyz#aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa$DE / backup / IPv4
##name: DE-Backup
##icon: 🇩🇪
##color: #2EBD85
##comment: reserve route, wbstream+datachannel does not work in guest flow
```

## Subscription client implementation

The main client that reads this subscription format is [owenewans/owenclave](https://github.com/owenewans/owenclave) ([src.owenewans.org/owenrtc](https://src.owenewans.org/owenrtc)) - an Android proxy client (fork of exclave) that supports all common protocols (vless, hysteria2, mieru, trojan, vmess, tuic, shadowsocks, socks ...) plus `olcrtc`, the `olcrtc://` URI format and subscriptions.

---

URI format for a single server: [uri.md](uri.md)

Compatibility matrix for auth + transport: [settings.md](settings.md)
