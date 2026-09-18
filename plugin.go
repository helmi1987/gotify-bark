package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/gotify/plugin-api"
)

// GetGotifyPluginInfo returns gotify plugin info.
func GetGotifyPluginInfo() plugin.Info {
	return plugin.Info{
		ModulePath:  "github.com/p3ddd/gotify-bark",
		Version:     "0.3.0",
		Author:      "Petrichor, extended by Benj Müller",
		Website:     "https://github.com/p3ddd/gotify-bark",
		License:     "MIT",
		Description: "Forwards Gotify messages to one or more Bark devices with priority based levels (critical/timeSensitive), groups and per-recipient rules.",
		Name:        "Bark Forwarder",
	}
}

// BarkForwardPlugin is the gotify plugin instance for forwarding messages to Bark.
type BarkForwardPlugin struct {
	// done channel is used to signal the goroutine to stop
	done chan struct{}
	// config holds the user-provided configuration
	config *Config
	// httpClient is a shared HTTP client with timeout for Bark and Gotify requests
	httpClient *http.Client
	// apps caches Gotify application names by id
	apps appCache
}

func logf(format string, args ...any) {
	log.Printf("Bark Forwarder: "+format, args...)
}

// Enable enables the plugin.
func (c *BarkForwardPlugin) Enable() error {
	if c.config == nil {
		return errors.New("plugin is not configured yet")
	}

	c.done = make(chan struct{})
	c.httpClient = &http.Client{Timeout: 30 * time.Second}
	c.apps = appCache{}

	go c.listenForMessages()

	logf("plugin enabled.")
	return nil
}

// Disable disables the plugin.
func (c *BarkForwardPlugin) Disable() error {
	if c.done != nil {
		close(c.done)
	}
	logf("plugin disabled.")
	return nil
}

// GetDisplay implements plugin.Displayer.
// This method provides instructions on the plugin's page in the Gotify UI.
func (c *BarkForwardPlugin) GetDisplay(location *url.URL) string {
	return `
### Bark Forwarder – Anleitung

Das Plugin verbindet sich als WebSocket-Client mit dem Gotify-Stream dieses Benutzers und leitet jede Nachricht an einen oder mehrere Bark-Empfänger weiter.
Die Gotify-Priorität bestimmt dabei den Bark-Level (passive / active / timeSensitive / critical) und bei Critical Alerts die Lautstärke.

**Wichtig:** Nach jeder Änderung der Konfiguration das Plugin **deaktivieren** und wieder **aktivieren**.

#### Verbindung

- **gotify_host**: WebSocket-Adresse des Gotify-Servers, z. B. ` + "`ws://192.168.1.10:8080`" + ` bzw. ` + "`wss://gotify.example.ch`" + ` bei HTTPS.
- **gotify_client_token**: Unter *Clients* einen neuen Client (z. B. ` + "`bark-plugin`" + `) anlegen und dessen Token hier eintragen. Der Token wird auch benutzt, um die App-Namen für die Gruppen abzufragen.
- **gotify_http_url**: Optional. HTTP-Adresse des Gotify-Servers für die API-Abfrage der App-Namen. Leer = wird aus gotify_host abgeleitet.
- **bark_url**: Bark-Server, normalerweise ` + "`https://api.day.app/push`" + `.
- **reconnect_delay**: Wartezeit in Sekunden vor einem Reconnect (Standard 10).

#### Prioritäts-Mapping (levels)

- Priorität ≤ **passive_max** (Standard 0) → ` + "`passive`" + ` – still, nur in der Mitteilungsliste
- dazwischen → ` + "`active`" + ` – normale Mitteilung
- Priorität ≥ **timesensitive_from** (Standard 8) → ` + "`timeSensitive`" + ` – durchbricht Fokus-Modi
- Priorität ≥ **critical_from** (Standard 10) → ` + "`critical`" + ` – Critical Alert, volume = Priorität − critical_from + 1 (max. 10)

Beispiel mit Standardwerten: Priorität 10 → critical, Lautstärke 1; 11 → Lautstärke 2; … 19 → Lautstärke 10.

#### Empfänger (recipients)

Jeder Eintrag ist ein Bark-Gerät mit eigenen Regeln:

- **name**: Nur für das Log.
- **device_key**: Bark Device Key.
- **min_priority**: Nachrichten mit tieferer Priorität werden nicht weitergeleitet.
- **max_level**: Deckel für den Bark-Level (passive, active, timeSensitive, critical). Gilt auch, wenn eine Nachricht per bark::params einen höheren Level verlangt.
- **apps**: Liste von Gotify-App-Namen (oder App-IDs). Leer = alle Apps.
- **group**: Feste Bark-Gruppe für diesen Empfänger. Leer = Gruppe aus ` + "`groups`" + ` bzw. App-Name.
- **params**: Zusätzliche Bark-Parameter, die immer mitgeschickt werden (z. B. sound, icon, isArchive).
- **levels**: Eigene Schwellen (passive_max, timesensitive_from, critical_from) für diesen Empfänger.
- **encryption_key / encryption_iv**: AES-CBC-Verschlüsselung (Key 16 oder 32 Bytes, IV 16 Bytes). Leer = unverschlüsselt.

#### Gruppen

- **group_from_app** (Standard true): Der Name der Gotify-App wird als Bark-Gruppe verwendet.
- **groups**: Zuordnung App-Name → Bark-Gruppe, z. B. ` + "`Uptime-Kuma: Monitoring`" + `.

#### Steuerung pro Nachricht

Alle Bark-Parameter lassen sich pro Nachricht über das Extra ` + "`bark::params`" + ` setzen und haben Vorrang vor dem Mapping:

` + "```" + `
{"title":"Backup","message":"fehlgeschlagen","priority":5,
 "extras":{"bark::params":{"level":"timeSensitive","group":"Backup","sound":"alarm"}}}
` + "```" + `

#### Beispiel-Konfiguration

` + "```yaml" + `
gotify_host: ws://gotify:80
gotify_client_token: <client token>
bark_url: hhttp://bark-server:8080/push
reconnect_delay: 10
levels:
  passive_max: 0
  timesensitive_from: 8
  critical_from: 10
group_from_app: true
groups:
  Uptime-Kuma: Monitoring
recipients:
  - name: User1
    device_key: <device key>
    min_priority: 0
  - name: User2
    device_key: <device key>
    min_priority: 5
    max_level: timeSensitive
    apps: [Alarmanlage, Uptime-Kuma]
    params:
      sound: minuet
` + "```" + `
`
}

// listenForMessages connects to the Gotify WebSocket and forwards messages.
// It runs in a loop to handle reconnections automatically.
func (c *BarkForwardPlugin) listenForMessages() {
	defer logf("listener stopped.")

	wsURL := c.config.GotifyHost + "/stream?token=" + c.config.GotifyClientToken

	for {
		// Check for disable signal before attempting to connect.
		select {
		case <-c.done:
			return
		default:
		}

		logf("connecting to %s", c.config.GotifyHost+"/stream")
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			logf("WebSocket dial error: %v", err)
			logf("reconnecting in %d seconds...", c.config.ReconnectDelay)
			select {
			case <-time.After(time.Duration(c.config.ReconnectDelay) * time.Second):
				continue
			case <-c.done:
				return
			}
		}

		logf("connected to Gotify WebSocket.")

		err = c.readMessages(conn)
		_ = conn.Close()

		if err == nil {
			// clean shutdown requested
			return
		}

		logf("disconnected: %v", err)

		select {
		case <-time.After(time.Duration(c.config.ReconnectDelay) * time.Second):
			continue
		case <-c.done:
			return
		}
	}
}

// readMessages reads messages from an active WebSocket connection.
// It returns an error if the connection is broken, or nil if it's cleanly closed via the 'done' channel.
func (c *BarkForwardPlugin) readMessages(conn *websocket.Conn) error {
	// A separate goroutine monitors the done channel and closes the connection.
	// This avoids the "repeated read on failed websocket connection" panic
	// that occurs when using ReadDeadline timeouts.
	closedByDone := make(chan struct{})
	var closeOnce sync.Once
	safeClose := func() {
		closeOnce.Do(func() { close(closedByDone) })
	}
	go func() {
		select {
		case <-c.done:
			logf("received disable signal, closing WebSocket.")
			_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			_ = conn.Close()
			safeClose()
		case <-closedByDone:
		}
	}()
	defer safeClose()

	for {
		_, messageBytes, err := conn.ReadMessage()
		if err != nil {
			select {
			case <-c.done:
				return nil // clean shutdown
			default:
			}
			return err
		}

		var msg streamMessage
		if err := json.Unmarshal(messageBytes, &msg); err != nil {
			logf("error unmarshalling message: %v", err)
			continue
		}

		c.forwardToBark(msg)
	}
}

// NewGotifyPluginInstance creates a plugin instance.
func NewGotifyPluginInstance(ctx plugin.UserContext) plugin.Plugin {
	return &BarkForwardPlugin{}
}

func main() {
	// This plugin is not meant to be run as a standalone application.
	// It should be built as a Go plugin and loaded by Gotify.
	panic("this should be built as go plugin")
}
