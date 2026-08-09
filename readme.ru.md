<!-- If you are an AI agent, please read agents.md -->

<div align="center">

<img src="docs/asset/westand.svg" width="250" height="250">

<br>

<img src="https://github.com/openlibrecommunity/material/blob/master/olcrtc.png" width="250" height="250">

<br>
<br>

<img src="https://count.owenewans.org/openlibrecommunity/olcrtc?theme=moebooru&notitle">

</div>

# olcRTC

[RU](readme.ru.md) / **EN**


`olcRTC` (OpenLibreCommunity RTC) - зашифрованный TCP-over-WebRTC туннель. Трафик маскируется под обычный видеозвонок на разрешённых сервисах (Jitsi, Yandex Telemost, WbStream). Внутри - шифрование XChaCha20-Poly1305 и мультиплексирование smux поверх WebRTC data/video каналов.

Статус: **Beta**

```text
app -> SOCKS5 -> olcrtc cnc -> WebRTC/SFU сервис -> olcrtc srv -> интернет
```

> **Важно:** проверяйте, что нужный сервис видеозвонков есть в белых списках и работает в вашей сети. Если нет - используйте другой.

## Возможности

- **Провайдеры:** `jitsi`, `telemost`, `wbstream`
- **Транспорты:** `datachannel`, `vp8channel`, `seichannel`, `videochannel`
- **Платформы:** Linux, macOS, Windows, Android (gomobile), встраиваемая Go-библиотека

Рекомендуемый старт: `jitsi + datachannel`.

## Быстрый старт

Сгенерируй общий ключ (одинаковый на сервере и клиенте):

```sh
openssl rand -hex 32
```

Нужны Podman и git.

```sh
git clone https://github.com/openlibrecommunity/olcrtc --recurse-submodules
cd olcrtc
./scripts/srv.sh
```


Полные инструкции в [docs/fast.md](docs/fast.ru.md) и [docs/manual.md](docs/manual.ru.md).

## Документация

| Документ | Содержание |
|---|---|
| [about.md](docs/about.ru.md) | архитектура, провайдеры, транспорты, публичный API |
| [fast.md](docs/fast.ru.md) | быстрый старт для новичков |
| [manual.md](docs/manual.ru.md) | ручная сборка |
| [configuration.md](docs/configuration.ru.md) | настройка YAML |
| [settings.md](docs/settings.ru.md) | матрица совместимости |
| [uri.md](docs/uri.ru.md) | формат URI клиента |
| [sub.md](docs/sub.ru.md) | формат подписки |

## Сборка

```sh
mage build   # текущая платформа
mage cross   # кросс-компиляция
mage test    # тесты
mage lint    # golangci-lint
mage mobile  # gomobile bindings (Android)
```

## Клиенты

- **Основной клиент:** [owenewans/owenclave](https://github.com/owenewans/owenclave) ([src.owenewans.org/owenrtc](https://src.owenewans.org/owenrtc)) - Android-клиент прокси (форк exclave). Поддерживает все распространённые протоколы (vless, hysteria2, mieru, trojan, vmess, tuic, shadowsocks, socks ...) плюс `olcrtc`, формат URI `olcrtc://` и подписки
- Клиенты сообщества:
  - [venterum/veil](https://github.com/venterum/veil) - V2Ray/Xray клиент для Android (форк v2rayNG), Material 3. Протоколы: VMess, VLESS, Shadowsocks, Trojan, SOCKS, WireGuard, Hysteria2 + `olcrtc`
  - [alananisimov/olcbox](https://github.com/alananisimov/olcbox) - Мультиплатформенный UI-клиент (Android, iOS, macOS, Windows, Linux). Kotlin Multiplatform/Compose. Все провайдеры (Jitsi, Telemost, WB Stream, Jazz), все транспорты, split tunneling, режимы TUN/proxy

## Сообщество

- Telegram: [@openlibrecommunity](https://t.me/openlibrecommunity)
- Issues: [github.com/openlibrecommunity/olcrtc/issues](https://github.com/openlibrecommunity/olcrtc/issues)

## Лицензия

WTFPL

<div align="center">

---

Telegram: [zarazaex](https://t.me/zarazaexe)
<br>
Email: [zarazaex@tuta.io](mailto:zarazaex@tuta.io)
<br>
Site: [zarazaex.xyz](https://zarazaex.xyz)

</div>
