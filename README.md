# gotify-bark

Gotify-Plugin, das Nachrichten an [Bark](https://bark.day.app) (iOS) weiterleitet.

Fork von [p3ddd/gotify-bark](https://github.com/p3ddd/gotify-bark), erweitert um:

- **Prioritäts-Mapping**: Die Gotify-Priorität bestimmt den Bark-Level (`passive`, `active`, `timeSensitive`, `critical`) und bei Critical Alerts die Lautstärke.
- **Mehrere Empfänger** pro Gotify-Benutzer, jeder mit eigenem Device Key, Mindestpriorität, Level-Deckel, App-Filter, Zusatzparametern und optionaler Verschlüsselung.
- **Gruppen**: Standardmässig der Name der Gotify-App, pro App oder pro Empfänger überschreibbar.
- Alles weiterhin pro Nachricht über das Extra `bark::params` übersteuerbar.

## Funktionsweise

Das Plugin verbindet sich mit einem Client-Token als WebSocket-Client an `/stream` des Gotify-Servers dieses Benutzers und schickt jede Nachricht per HTTP an den Bark-Server. Für die Gruppen holt es die App-Namen einmal über `GET /application` (gleicher Token) und cached sie.

### Prioritäts-Mapping

| Gotify-Priorität (Standard) | Bark-Level | Bemerkung |
|---|---|---|
| ≤ 0 (`passive_max`) | `passive` | still, nur in der Mitteilungsliste |
| 1–7 | `active` | normale Mitteilung |
| 8–9 (`timesensitive_from`) | `timeSensitive` | durchbricht Fokus-Modi |
| ≥ 10 (`critical_from`) | `critical` | Critical Alert, `volume = Priorität − critical_from + 1`, maximal 10 |

Mit den Standardwerten: Priorität 10 → Lautstärke 1, 11 → 2, … 19 → 10, alles darüber bleibt bei 10.

Die Schwellen sind global unter `levels` und pro Empfänger unter `recipients[].levels` einstellbar.

### Reihenfolge der Regeln

1. `bark::params` aus den Extras der Nachricht (höchste Priorität)
2. Level/Volume aus der Priorität, Gruppe aus `groups` bzw. App-Name
3. `recipients[].params` (statische Defaults wie `sound`, `icon`, `isArchive`)

`max_level` eines Empfängers gilt danach als harter Deckel, auch gegenüber `bark::params`. Der Device Key kann nicht per Nachricht überschrieben werden.

## Konfiguration

Im Gotify-UI unter *Plugins → Bark Forwarder → Configurer*. Nach jeder Änderung das Plugin deaktivieren und wieder aktivieren.

```yaml
gotify_host: ws://localhost:80          # wss:// bei HTTPS
gotify_client_token: <client token>     # Gotify → Clients → Create Client
gotify_http_url: ""                     # optional, sonst aus gotify_host abgeleitet
bark_url: https://api.day.app/push
reconnect_delay: 10

levels:                                 # globale Schwellen
  passive_max: 0
  timesensitive_from: 8
  critical_from: 10

group_from_app: true                    # App-Name als Bark-Gruppe
groups:                                 # App-Name (oder App-ID) → Gruppe
  Uptime-Kuma: Monitoring

recipients:
  - name: benj
    device_key: <bark device key>
    min_priority: 0
  - name: partner
    device_key: <bark device key>
    min_priority: 5                     # darunter nichts weiterleiten
    max_level: timeSensitive            # bekommt nie einen Critical Alert
    apps: [Alarmanlage, Uptime-Kuma]    # nur diese Gotify-Apps
    group: Zuhause                      # feste Gruppe statt App-Name
    params:                             # immer mitgeschickte Bark-Parameter
      sound: minuet
    levels:                             # eigene Schwellen
      timesensitive_from: 6
    encryption_key: ""                  # 16 oder 32 Bytes, leer = unverschlüsselt
    encryption_iv: ""                   # 16 Bytes
```

Eine alte Konfiguration mit `bark_device_key` funktioniert weiterhin und wird als Empfänger `default` behandelt.

### Beispiele

Normale Nachricht (Priorität 5 → `active`, Gruppe = App-Name):

```sh
curl "https://gotify.example.ch/message?token=<app token>" \
  -F "title=Backup" -F "message=erfolgreich" -F "priority=5"
```

Critical Alert mit Lautstärke 6 (Priorität 15):

```sh
curl "https://gotify.example.ch/message?token=<app token>" \
  -F "title=Server down" -F "message=nostromo antwortet nicht" -F "priority=15"
```

Time Sensitive, eigene Gruppe und Klingelton per Extra, unabhängig von der Priorität:

```sh
curl "https://gotify.example.ch/message?token=<app token>" \
  -H "Content-Type: application/json" \
  -d '{"title":"Tür","message":"Haustür offen","priority":3,
       "extras":{"bark::params":{"level":"timeSensitive","group":"Zuhause","sound":"alarm"}}}'
```

Alle Bark-Parameter (`sound`, `icon`, `url`, `call`, `badge`, `isArchive`, `copy`, …) sind in der [Bark-Dokumentation](https://bark.day.app) beschrieben.

## Build

Gotify lädt nur Plugins, die mit exakt derselben Go-Version und denselben Modul-Versionen wie der Server gebaut wurden.

Mit Docker (offizieller Weg, `gotify/build`-Image):

```sh
make GOTIFY_VERSION=v3.1.1 build-linux-amd64
```

Ohne Docker auf einem Linux-Host mit gcc: Gotify prüft beim Laden einen Fingerabdruck jedes gemeinsam genutzten Pakets, und darin stecken auch die Quellpfade. Der Build muss deshalb dasselbe Layout wie das `gotify/build`-Image haben, also die passende Go-Version unter `/usr/local/go` und den Modul-Cache unter `/go/pkg/mod`. Das Target prüft die Go-Version und setzt den Cache-Pfad selbst:

```sh
# 1. Quellcode herunterladen
rm -rf /proj/*
rm -rf /proj/.*
git clone https://github.com/helmi1987/gotify-bark.git .

# 2. Abhängigkeiten laden
go mod tidy

# 3. Plugin kompilieren
go build -a -installsuffix cgo -ldflags "-w -s" -buildmode=plugin -o gotify-bark.so

# 4. Kompilierte Datei verschieben
cp gotify-bark.so /out/
```

Die `.so` landet in `build/` und wird in das Plugin-Verzeichnis von Gotify kopiert (Docker: `/app/data/plugins`). Danach Gotify neu starten.

## Tests

```sh
go test ./...
```
