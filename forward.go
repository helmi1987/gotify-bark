package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strconv"
	"strings"
)

// extrasKey is the Gotify extras key whose content is merged into the Bark request.
const extrasKey = "bark::params"

// maxBarkVolume is the highest volume Bark accepts for critical alerts.
const maxBarkVolume = 10

// streamMessage is the message as delivered by the Gotify /stream WebSocket.
// (plugin.Message from the plugin API has no app id, so we decode into our own type.)
type streamMessage struct {
	ID       uint           `json:"id"`
	AppID    uint           `json:"appid"`
	Message  string         `json:"message"`
	Title    string         `json:"title"`
	Priority int            `json:"priority"`
	Extras   map[string]any `json:"extras"`
	Date     string         `json:"date"`
}

// mapPriority converts a Gotify priority into a Bark level and (for critical) volume.
// volume is 0 when not applicable.
func mapPriority(priority int, lv effectiveLevels) (level string, volume int) {
	switch {
	case priority >= lv.criticalFrom:
		volume = priority - lv.criticalFrom + 1
		if volume > maxBarkVolume {
			volume = maxBarkVolume
		}
		return LevelCritical, volume
	case priority >= lv.timeSensitiveFrom:
		return LevelTimeSensitive, 0
	case priority <= lv.passiveMax:
		return LevelPassive, 0
	default:
		return LevelActive, 0
	}
}

// capLevel lowers level to maxLevel when it ranks higher. An empty maxLevel means no cap.
func capLevel(level, maxLevel string) string {
	if maxLevel == "" {
		return level
	}
	if levelRank[level] > levelRank[maxLevel] {
		return maxLevel
	}
	return level
}

// extrasParams returns the bark::params map from the message extras, or nil.
func extrasParams(extras map[string]any) map[string]any {
	if extras == nil {
		return nil
	}
	raw, ok := extras[extrasKey]
	if !ok {
		return nil
	}
	params, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	return params
}

// wantsMessage reports whether the recipient should receive the message, with a reason when not.
func (r *Recipient) wantsMessage(msg streamMessage, appName string) (bool, string) {
	if msg.Priority < r.MinPriority {
		return false, fmt.Sprintf("priority %d below min_priority %d", msg.Priority, r.MinPriority)
	}
	if r.apps != nil {
		_, byName := r.apps[strings.ToLower(appName)]
		_, byID := r.apps[strconv.FormatUint(uint64(msg.AppID), 10)]
		if !byName && !byID {
			return false, fmt.Sprintf("app %q (id %d) not in apps filter", appName, msg.AppID)
		}
	}
	return true, ""
}

// resolveGroup picks the Bark group: explicit extras > recipient.group > groups map > app name.
func (c *BarkForwardPlugin) resolveGroup(r *Recipient, msg streamMessage, appName string) string {
	if r.Group != "" {
		return r.Group
	}
	if c.config.Groups != nil {
		if g, ok := c.config.Groups[appName]; ok && g != "" {
			return g
		}
		if g, ok := c.config.Groups[strconv.FormatUint(uint64(msg.AppID), 10)]; ok && g != "" {
			return g
		}
	}
	if c.config.groupFromApp() && appName != "" {
		return appName
	}
	return ""
}

// buildBarkRequest assembles the Bark push body for one recipient.
//
// Precedence (highest first):
//  1. bark::params from the message extras (level, volume, group, sound, ...)
//  2. level/volume mapped from the Gotify priority, group from config/app name
//  3. recipient.params (static defaults such as sound or icon)
//
// max_level of the recipient is applied last and also caps an explicit extras level.
func (c *BarkForwardPlugin) buildBarkRequest(r *Recipient, msg streamMessage, appName string) map[string]any {
	req := map[string]any{}

	// 3. static recipient defaults
	maps.Copy(req, r.Params)

	// 2. mapping from priority and group resolution
	req["title"] = msg.Title
	req["body"] = msg.Message
	level, volume := mapPriority(msg.Priority, r.levels)
	req["level"] = level
	if level == LevelCritical {
		req["volume"] = volume
	} else {
		delete(req, "volume")
	}
	if g := c.resolveGroup(r, msg, appName); g != "" {
		req["group"] = g
	}

	// 1. explicit per-message parameters
	maps.Copy(req, extrasParams(msg.Extras))

	// hard cap per recipient
	if lvl, ok := req["level"].(string); ok {
		capped := capLevel(lvl, r.MaxLevel)
		req["level"] = capped
		if capped != LevelCritical {
			delete(req, "volume")
		}
	}

	// the device key is never overridable from a message
	delete(req, "device_keys")
	req["device_key"] = r.DeviceKey
	return req
}

// forwardToBark sends the message to every matching recipient.
func (c *BarkForwardPlugin) forwardToBark(msg streamMessage) {
	appName := c.appName(msg.AppID)
	for i := range c.config.Recipients {
		r := &c.config.Recipients[i]
		ok, reason := r.wantsMessage(msg, appName)
		if !ok {
			logf("skipping recipient %q for message %d: %s", r.Name, msg.ID, reason)
			continue
		}
		req := c.buildBarkRequest(r, msg, appName)
		encrypted := r.EncryptionKey != "" && r.EncryptionIV != ""
		if before, after := fitPayload(req, encrypted, c.config.MaxPayload, c.config.truncateMarker()); after != before {
			logf("truncated body of message %d for recipient %q from %d to %d bytes (max_payload %d, encrypted=%t)",
				msg.ID, r.Name, before, after, c.config.MaxPayload, encrypted)
		}
		if err := c.sendToBark(r, req); err != nil {
			logf("failed to forward message %d to recipient %q: %v", msg.ID, r.Name, err)
			continue
		}
		logf("forwarded message %d (app %q, priority %d) to %q as level=%v group=%v",
			msg.ID, appName, msg.Priority, r.Name, req["level"], req["group"])
	}
}

// sendToBark posts one request (plain or encrypted) to the Bark server.
func (c *BarkForwardPlugin) sendToBark(r *Recipient, barkReq map[string]any) error {
	jsonValue, err := json.Marshal(barkReq)
	if err != nil {
		return fmt.Errorf("could not marshal bark request: %w", err)
	}

	var requestBody []byte
	var targetURL string
	if r.EncryptionKey != "" && r.EncryptionIV != "" {
		ciphertext, err := encryptAESCBC(r.EncryptionKey, r.EncryptionIV, jsonValue)
		if err != nil {
			return fmt.Errorf("encryption failed: %w", err)
		}
		requestBody, err = json.Marshal(map[string]string{"ciphertext": ciphertext})
		if err != nil {
			return fmt.Errorf("could not marshal encrypted request: %w", err)
		}
		targetURL = encryptedPushURL(c.config.BarkURL, r.DeviceKey)
	} else {
		requestBody = jsonValue
		targetURL = plainPushURL(c.config.BarkURL)
	}

	resp, err := c.httpClient.Post(targetURL, "application/json", bytes.NewReader(requestBody))
	if err != nil {
		return fmt.Errorf("http post to bark failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("bark server returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func plainPushURL(baseURL string) string {
	if baseURL == "" {
		return "https://api.day.app/push"
	}
	baseURL = strings.TrimSpace(baseURL)
	baseURL = strings.TrimRight(baseURL, "/")
	if strings.HasSuffix(baseURL, "/push") {
		return baseURL
	}
	return baseURL + "/push"
}

func encryptedPushURL(baseURL, deviceKey string) string {
	if baseURL == "" {
		baseURL = "https://api.day.app"
	}
	baseURL = strings.TrimSpace(baseURL)
	baseURL = strings.TrimSuffix(baseURL, "/push")
	baseURL = strings.TrimRight(baseURL, "/")
	return fmt.Sprintf("%s/%s", baseURL, deviceKey)
}
