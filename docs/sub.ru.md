<div align="center">

<img src="https://github.com/openlibrecommunity/material/blob/master/olcrtc.png" width="250" height="250">

![License](https://img.shields.io/badge/license-WTFPL-0D1117?style=flat-square&logo=open-source-initiative&logoColor=green&labelColor=0D1117)
![Golang](https://img.shields.io/badge/-Golang-0D1117?style=flat-square&logo=go&logoColor=00A7D0)

**RU** / [EN](sub.md)

</div>

# Формат подписки `sub.md`

`sub.md` это обычный текстовый файл, который хостится на сервере и отдаётся как plain text.

Пример URL:

```text
https://killpeople.freegore.xyz/sub
```

Внутри файл содержит список `olcrtc`-URI из [uri.md](uri.ru.md) и дополнительные технические поля для клиента.

Важно: это соглашение **для клиентских приложений**. Сам `olcrtc` такой файл не читает и не обрабатывает.

---

## Назначение

Формат нужен для клиентских подписок:

- список серверов в одном файле
- метаданные подписки для UI
- метаданные отдельных серверов
- информация для автообновления подписки

---

## Общая структура

Файл читается сверху вниз и состоит из:

1. глобальных полей подписки с префиксом `#`
2. строк `olcrtc://...`
3. локальных полей конкретного сервера с префиксом `##`

Базовая схема:

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

## Глобальные поля подписки

Строки вида `#key: value` относятся ко всей подписке.

| Поле | Значение |
|------|----------|
| `#name:` | Имя подписки |
| `#update:` | Время последнего обновления в Unix time |
| `#refresh:` | Через какой интервал клиенту нужно обновлять подписку, например `5s`, `10m`, `6h` |
| `#color:` | Цвет подписки. Поле только для UI |
| `#icon:` | Иконка подписки. Поле только для UI |
| `#used:` | Сколько уже использовано, например `10mb/10gb` |
| `#available:` | Сколько доступно всего по подписке, например `1.1gb` |

`#available:` это именно значение на уровне всей подписки. Если клиент умеет считать остаток сам, он может использовать это поле как исходные данные или как отображаемую подсказку.

---

## Строки серверов

Каждая строка сервера содержит один `olcrtc`-URI в формате из [uri.md](uri.ru.md):

```text
olcrtc://<Auth>?<Transport>@<RoomID>#<EncryptionKey>$<MIMO>
olcrtc://<Auth>?<Transport><key=value&key=value>@<RoomID>#<EncryptionKey>$<MIMO>
```

Одна строка = один сервер/одна запись подписки.

Пустые строки между элементами допустимы.

---

## Локальные поля сервера

Строки вида `##key: value` относятся только к **последнему URI**, который был объявлен выше.

То есть клиент должен привязывать блок `##...` к ближайшей предыдущей строке `olcrtc://...`.

| Поле | Значение |
|------|----------|
| `##name:` | Имя сервера/узла |
| `##color:` | Цвет для UI |
| `##icon:` | Иконка для UI |
| `##used:` | Использование для конкретного сервера, например `500mb/10gb` |
| `##available:` | Доступный объём для конкретного сервера |
| `##ip:` | IP-адрес сервера, если его нужно показать клиенту |
| `##comment:` | Свободный комментарий |

Локальные поля почти повторяют глобальные, но без `refresh`, потому что период обновления задаётся на уровне всей подписки.

## Рекомендации по значениям

- Для `#update:` использовать Unix time в секундах.
- Для `#refresh:` использовать короткие интервалы вида `5s`, `10m`, `6h`, `1d`.
- Для `#color:` использовать один стабильный формат в рамках клиента, например `#RRGGBB`.
- Для `#icon:` использовать строковый идентификатор или emoji.
- Для `#used:` и `#available:` использовать человекочитаемые единицы `kb`, `mb`, `gb`, `tb`.

---

## Полный пример

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

## Имплементация клиента для подписок

Основной клиент, который читает этот формат подписки - [owenewans/owenclave](https://github.com/owenewans/owenclave) ([src.owenewans.org/owenrtc](https://src.owenewans.org/owenrtc)) - Android-клиент прокси (форк exclave), поддерживающий все распространённые протоколы (vless, hysteria2, mieru, trojan, vmess, tuic, shadowsocks, socks ...) плюс `olcrtc`, формат URI `olcrtc://` и подписки.

---

URI-формат для отдельного сервера: [uri.md](uri.ru.md)

Матрица совместимости auth + transport: [settings.md](settings.ru.md)
