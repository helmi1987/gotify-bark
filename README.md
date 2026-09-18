# gotify-bark

Gotify-Plugin, das Nachrichten an [Bark](https://bark.day.app) (iOS) weiterleitet.

Fork von [p3ddd/gotify-bark](https://github.com/p3ddd/gotify-bark), erweitert um:

- **Prioritäts-Mapping**: Die Gotify-Priorität bestimmt den Bark-Level (`passive`, `active`, `timeSensitive`, `critical`) und bei Critical Alerts die Lautstärke.
- **Mehrere Empfänger** pro Gotify-Benutzer, jeder mit eigenem Device Key, Mindestpriorität, Level-Deckel, App-Filter, Zusatzparametern und optionaler Verschlüsselung.
- **Gruppen**: Standardmässig der Name der Gotify-App, pro App oder pro Empfänger überschreibbar.
- Alles weiterhin pro Nachricht über das Extra `bark::params` übersteuerbar.
- **Längenbegrenzung**: Lange Nachrichten werden so gekürzt, dass Apple sie annimmt (4096-Byte-Limit), statt verloren zu gehen. Verschlüsselungs-Overhead wird mitgerechnet.

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

### Nachrichtenlänge

Apple (APNs) akzeptiert pro Push maximal 4096 Bytes für das gesamte Payload, inklusive Bark-Parametern und Apples eigenem Rahmen (`aps` mit alert, sound, category, thread-id). Der Bark-Server kürzt nicht; Apple antwortet mit `413 PayloadTooLarge` und die Nachricht kommt nie auf dem iPhone an.

Das Plugin misst deshalb vor dem Senden die Grösse des Bark-JSON und kürzt bei Bedarf nur den Nachrichtentext (`body`), an einer Zeichengrenze, mit einem Marker am Ende. Titel, Level, Lautstärke und Gruppe bleiben unangetastet, ein Critical Alert kommt also immer. Jede Kürzung steht im Gotify-Log (`truncated body of message …`).

Bei verschlüsselten Empfängern zählt nicht der Klartext, sondern der base64-Ciphertext: Klartext → PKCS7-Padding auf das nächste 16er-Vielfache → base64 × 4/3. Das kostet rund 1 KB Text.

| Modus | Budget (Standard `max_payload: 3800`) | davon reiner Nachrichtentext (ungefähr) |
|---|---|---|
| unverschlüsselt | 3800 Bytes JSON | ca. 3500 Bytes |
| verschlüsselt | ca. 2700 Bytes Klartext-JSON → ca. 3600 Bytes base64 | ca. 2500 Bytes |

Umlaute zählen in UTF-8 doppelt, JSON-Escapes (`"`, `<`, `>`, Zeilenumbrüche) ebenfalls mehr als ein Byte.

```yaml
max_payload: 3800                       # Bytes, 512–4096, 0 = Standard
truncate_marker: " … [gekürzt, vollständig in Gotify]"   # "" = kein Marker
```

Das Budget gilt für das **ganze** Bark-JSON, nicht nur für den Text: `device_key`, Titel, `level`, `volume`, `group` und die JSON-Struktur brauchen zusammen rund 150–250 Bytes, der Marker nochmals 37. Werte unter 512 lehnt das Plugin deshalb ab, denn dann bliebe für den Text nichts übrig. Zum Ausprobieren der Kürzung eignet sich `max_payload: 600`: rund 400 Zeichen Text plus Marker.

Tipp: Mit `params: {url: https://gotify.example.ch}` beim Empfänger öffnet ein Tipp auf die Mitteilung den vollständigen Text in Gotify.

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

max_payload: 3800                       # Grössenbudget in Bytes (siehe Nachrichtenlänge)
# truncate_marker: " …"                 # eigener Marker, "" = keiner

recipients:
  - name: Device1
    device_key: <bark device key>
    min_priority: 0
  - name: Device2
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

Gotify lädt nur Plugins, die mit exakt derselben Go-Version und denselben Modul-Versionen wie der Server gebaut wurden. Passt etwas nicht, startet Gotify gar nicht mehr (`plugin was built with a different version of package …`). Deshalb:

- `go.mod` ist auf das `go.mod` von Gotify **v3.1.1** abgeglichen (Go 1.26.0). Bei einem Gotify-Update `make GOTIFY_VERSION=vX.Y.Z update-go-mod` ausführen und neu bauen.
- Das Gotify-Image in `docker-compose.yaml` ist auf die Version gepinnt, für die das Plugin gebaut wurde. Ein `latest` würde beim nächsten Pull das Plugin und damit Gotify lahmlegen.
- Gotify prüft beim Laden einen Fingerabdruck jedes gemeinsam genutzten Pakets, und darin stecken auch die Quellpfade. Der Build muss deshalb dasselbe Layout wie das `gotify/build`-Image haben (Go unter `/usr/local/go`, Modul-Cache unter `/go/pkg/mod`). Am einfachsten baut man direkt in diesem Image.

### Im Builder-Container (docker-compose) – empfohlen

Die `docker-compose.yaml` enthält den Dienst `plugin-builder` (`gotify/build:1.26.0-linux-amd64`), der `/opt/gotify/tmp/proj` als Arbeitsverzeichnis und den Plugin-Ordner von Gotify als `/out` einbindet.

```sh
docker compose up -d plugin-builder
docker exec -it gotify-builder bash
```

Im Container einmalig den Quellcode holen, danach genügt `git pull`:

```sh
rm -rf /proj/* /proj/.[!.]*
git clone https://github.com/helmi1987/gotify-bark.git .
```

Bauen mit `build.sh`. Das Script fragt die **laufende Gotify** nach ihrer Version (`GET /version`), lädt zu genau diesem Tag `GO_VERSION` und `go.mod` von GitHub, prüft, ob die Go-Version im Container passt, gleicht `go.mod` ab und legt die `.so` nach `/out`:

```sh
./build.sh                # Version von http://gotify:80 holen, bauen, nach /out kopieren
DRY_RUN=1 ./build.sh      # nur prüfen, ob alles zusammenpasst
./build.sh -h             # alle Umgebungsvariablen (GOTIFY_URL, GOTIFY_VERSION, OUT_DIR, …)
```

Passt die Go-Version nicht, bricht das Script ab und nennt das richtige `gotify/build`-Image. Meldet es, dass `go.mod`/`go.sum` geändert wurden, hat die laufende Gotify andere Modulversionen als das Repo: Änderung committen, damit der nächste Build ohne Netz auskommt.

Danach Gotify neu starten (`docker compose restart gotify`). Im Gotify-UI unter *Plugins* erscheint «Bark Forwarder» mit der Versionsnummer, im Log `Bark Forwarder v0.3.1 (…) enabled`.

Von Hand ohne Script (entspricht dem, was `build.sh` macht, aber ohne Versionsprüfung):

```sh
go mod tidy
go build -a -installsuffix cgo -ldflags "-w -s" -buildmode=plugin -o gotify-bark.so
cp gotify-bark.so /out/
```

### Mit dem Makefile

Mit Docker auf dem Host (zieht das passende `gotify/build`-Image selbst):

```sh
make GOTIFY_VERSION=v3.1.1 build-linux-amd64
```

Ohne Docker auf einem Linux-Host mit gcc, Go 1.26.0 unter `/usr/local/go` (das Target prüft die Go-Version und setzt den Cache-Pfad):

```sh
make GOTIFY_VERSION=v3.1.1 build-local
```

Die `.so` landet in `build/` und wird nach `/app/data/plugins` im Gotify-Container kopiert (`/opt/gotify/data/plugins` auf dem Host). Danach Gotify neu starten.

## Version prüfen

Eine `.so` lässt sich nicht mit `--version` aufrufen, die Version ist aber an drei Stellen sichtbar:

- Gotify-UI → *Plugins*: Versionsspalte beim «Bark Forwarder», ebenso oben in der Plugin-Anleitung.
- Gotify-Log beim Aktivieren: `Bark Forwarder v0.3.1 (…) enabled (2 recipient(s), max_payload 3800).`
- In der Datei selbst, ohne Gotify: `strings gotify-bark.so | grep "Bark Forwarder v"` (im Container: `docker exec gotify sh -c 'strings /app/data/plugins/gotify-bark.so | grep "Bark Forwarder v"'`, falls `strings` fehlt: `grep -a -o "Bark Forwarder v[0-9.]*" gotify-bark.so`).

Die Version steht an genau einer Stelle im Code (`pluginVersion` in `plugin.go`).

## Tests

```sh
go test ./...
```
