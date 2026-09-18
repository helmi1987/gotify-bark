# Uptime Kuma → Gotify → Bark: Critical Alerts mit Priorität über 10

Die eingebaute Gotify-Benachrichtigung in Uptime Kuma erlaubt im Formular nur Priorität 0–10. Mit 10 gibt das Bark-Plugin bereits `critical` mit Lautstärke 1. Für alles darüber (und für `bark::params`) wird statt «Gotify» der Typ **Webhook** verwendet, der die Gotify-API direkt anspricht.

## Prioritäts-Mapping des Plugins (Standardwerte)

| Gotify-Priorität | Bark |
|---|---|
| ≤ 0 | `passive` – still, nur in der Mitteilungsliste |
| 1–7 | `active` – normale Mitteilung |
| 8–9 | `timeSensitive` – durchbricht Fokus-Modi |
| 10–19 | `critical` – Lautstärke = Priorität − 9 (10 → 1, 11 → 2, … 19 → 10) |
| ≥ 20 | `critical` – Lautstärke 10 |

## Einrichtung in Uptime Kuma

*Settings → Notifications → Setup Notification*

| Feld | Wert |
|---|---|
| Notification Type | Webhook |
| Friendly Name | z. B. `iPhone Stefan Critical (vol_2)` |
| Post URL | `https://temp.helminet.ch/message` |
| Request Body | Custom Body |
| Additional Headers | `{"X-Gotify-Key": "<Application Token>"}` |

### Custom Body – feste Priorität

```json
{
  "title": {{ monitorJSON.name | json }},
  "message": {{ msg | json }},
  "priority": 11,
  "extras": {
    "bark::params": {
      "group": "Uptime-Kuma"
    }
  }
}
```

Die `{{ … }}`-Platzhalter sind Liquid-Templates von Uptime Kuma (ab Version 1.23). Der Filter `| json` setzt die Anführungszeichen und escaped den Text; ohne ihn zerlegt ein Anführungszeichen in der Fehlermeldung das JSON.

`group` kann weggelassen werden, dann nimmt das Plugin den Namen der Gotify-App als Bark-Gruppe.

### Custom Body – Down laut, Up leise (empfohlen)

Ohne Unterscheidung kommt auch «wieder erreichbar» als Critical Alert. Mit einer Bedingung im Body (Status 0 = Down, 1 = Up) geht beides in einer Benachrichtigung:

```json
{
  "title": {{ monitorJSON.name | json }},
  "message": {{ msg | json }},
  "priority": {% if heartbeatJSON and heartbeatJSON.status == 0 %}11{% else %}5{% endif %}
}
```

- Down → Priorität 11 → `critical`, Lautstärke 2
- Up → Priorität 5 → normale Mitteilung
- `heartbeatJSON and` fängt Meldungen ohne Heartbeat ab (Test-Button, Zertifikatswarnungen); die landen bei 5.

Die Lautstärke wird über die Priorität gesteuert: 12 → Lautstärke 3, 15 → Lautstärke 6, 19 → Lautstärke 10.

## Weitere Bark-Parameter pro Benachrichtigung

Alles, was Bark kennt, kann in `bark::params` mitgegeben werden und hat Vorrang vor dem Mapping:

```json
"extras": {
  "bark::params": {
    "level": "timeSensitive",
    "group": "Monitoring",
    "sound": "alarm",
    "url": "https://uptime.example.ch"
  }
}
```

Ein `max_level` beim Empfänger in der Plugin-Config bleibt trotzdem der Deckel.

## Testen und Fehlersuche

- Button **Test** in Uptime Kuma zeigt sofort, ob der Aufruf ankommt.
- **401** → Application Token im Header stimmt nicht.
- **400** → JSON kaputt, meist fehlt der `| json`-Filter oder ein Komma.
- Kommt die Nachricht in Gotify an, aber nicht auf dem iPhone: Plugin-Log in Gotify prüfen (`Bark Forwarder: forwarded message …` bzw. `skipping recipient …`).
- Critical Alerts brauchen in den iOS-Einstellungen von Bark die Berechtigung «Kritische Hinweise»; die App fragt beim ersten Critical Alert danach.

## Direkt per curl testen

```sh
# critical, Lautstärke 2
curl "https://temp.helminet.ch/message?token=<Application Token>" \
  -F "title=Test" -F "message=critical vol 2" -F "priority=11"

# timeSensitive mit eigener Gruppe
curl "https://temp.helminet.ch/message?token=<Application Token>" \
  -H "Content-Type: application/json" \
  -d '{"title":"Test","message":"time sensitive","priority":8,
       "extras":{"bark::params":{"group":"Test"}}}'
```
