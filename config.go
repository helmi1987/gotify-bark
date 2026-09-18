package main

import (
	"crypto/aes"
	"errors"
	"fmt"
	"strings"
)

// Bark notification levels, see https://bark.day.app
const (
	LevelPassive       = "passive"
	LevelActive        = "active"
	LevelTimeSensitive = "timeSensitive"
	LevelCritical      = "critical"
)

// levelRank orders the Bark levels so that "max_level" can cap a mapped level.
var levelRank = map[string]int{
	LevelPassive:       0,
	LevelActive:        1,
	LevelTimeSensitive: 2,
	LevelCritical:      3,
}

// Config defines the plugin config scheme (edited as YAML in the Gotify UI).
type Config struct {
	// The WebSocket URL of the Gotify server, e.g. "ws://gotify:80" (container name from docker-compose.yaml)
	GotifyHost string `yaml:"gotify_host"`
	// A client token from Gotify for the plugin to use
	GotifyClientToken string `yaml:"gotify_client_token"`
	// Optional HTTP(S) URL of the Gotify server used to resolve application names
	// (GET /application). Derived from gotify_host when empty (ws -> http, wss -> https).
	GotifyHTTPURL string `yaml:"gotify_http_url,omitempty"`
	// The Bark server push URL
	BarkURL string `yaml:"bark_url"`
	// ReconnectDelay is the delay in seconds before trying to reconnect.
	ReconnectDelay int `yaml:"reconnect_delay,omitempty"`

	// Legacy single-recipient settings (kept for backwards compatibility).
	// If "recipients" is empty, these are turned into one recipient named "default".
	BarkDeviceKey string `yaml:"bark_device_key,omitempty"`
	EncryptionKey string `yaml:"encryption_key,omitempty"`
	EncryptionIV  string `yaml:"encryption_iv,omitempty"`

	// Levels maps the Gotify priority to a Bark level (defaults for all recipients).
	Levels LevelConfig `yaml:"levels"`
	// GroupFromApp uses the Gotify application name as Bark group when no other group applies.
	// nil (key missing) means true.
	GroupFromApp *bool `yaml:"group_from_app"`
	// Groups overrides the Bark group per Gotify application name (or numeric app id as string).
	Groups map[string]string `yaml:"groups,omitempty"`
	// Recipients is the list of Bark devices to forward to, each with its own rules.
	Recipients []Recipient `yaml:"recipients"`

	// MaxPayload is the size budget in bytes for the JSON sent to Bark (for encrypted recipients:
	// the base64 ciphertext). Apple rejects APNs payloads above 4096 bytes including its own
	// framing, so the default keeps a reserve. Longer message bodies are truncated. 0 = default.
	MaxPayload int `yaml:"max_payload,omitempty"`
	// TruncateMarker is appended to a truncated body. nil (key missing) = default marker, "" = none.
	TruncateMarker *string `yaml:"truncate_marker,omitempty"`
}

// Defaults match the service names in docker-compose.yaml: the plugin runs inside the
// gotify container and reaches Gotify itself and the bark-server over the compose network.
const (
	defaultGotifyHost = "ws://gotify:80"
	defaultBarkURL    = "http://bark-server:8080/push"
)

// defaultMaxPayload leaves roughly 300 bytes of the 4096-byte APNs limit for Apple's framing
// (aps dictionary with alert, sound, category, mutable-content, thread-id).
const defaultMaxPayload = 3800

// minMaxPayload rejects budgets that cannot hold anything but the fixed fields.
const minMaxPayload = 512

// defaultTruncateMarker is appended to bodies that had to be shortened.
const defaultTruncateMarker = " … [gekürzt, vollständig in Gotify]"

// truncateMarker returns the configured marker or the default.
func (c *Config) truncateMarker() string {
	if c.TruncateMarker == nil {
		return defaultTruncateMarker
	}
	return *c.TruncateMarker
}

// LevelConfig defines the priority thresholds for the Bark level mapping.
//
//	priority <= passive_max                     -> passive
//	passive_max < priority < timesensitive_from -> active
//	timesensitive_from <= priority < critical_from -> timeSensitive
//	priority >= critical_from                   -> critical, volume = priority - critical_from + 1 (max 10)
type LevelConfig struct {
	PassiveMax        *int `yaml:"passive_max,omitempty"`
	TimeSensitiveFrom *int `yaml:"timesensitive_from,omitempty"`
	CriticalFrom      *int `yaml:"critical_from,omitempty"`
}

// Recipient is one Bark device with its own filter and mapping rules.
type Recipient struct {
	// Name is only used for logging.
	Name string `yaml:"name"`
	// DeviceKey is the Bark device key.
	DeviceKey string `yaml:"device_key"`
	// MinPriority: messages with a lower Gotify priority are not forwarded to this recipient.
	MinPriority int `yaml:"min_priority"`
	// MaxLevel caps the Bark level for this recipient (passive, active, timeSensitive, critical). Empty = no cap.
	MaxLevel string `yaml:"max_level,omitempty"`
	// Apps restricts forwarding to these Gotify application names (or numeric ids as string). Empty = all.
	Apps []string `yaml:"apps,omitempty"`
	// Group is a fixed Bark group for this recipient. Empty = use groups map / app name.
	Group string `yaml:"group,omitempty"`
	// Params are additional Bark parameters always sent to this recipient (e.g. sound, icon, isArchive).
	Params map[string]any `yaml:"params,omitempty"`
	// Levels overrides the global priority mapping for this recipient.
	Levels *LevelConfig `yaml:"levels,omitempty"`
	// EncryptionKey is the AES key (16 bytes AES-128 or 32 bytes AES-256). Empty = plain push.
	EncryptionKey string `yaml:"encryption_key,omitempty"`
	// EncryptionIV is the AES-CBC IV (16 bytes).
	EncryptionIV string `yaml:"encryption_iv,omitempty"`

	// apps holds the lower-cased app filter for fast lookups (filled during validation).
	apps map[string]struct{}
	// levels holds the effective (merged) level thresholds (filled during validation).
	levels effectiveLevels
}

type effectiveLevels struct {
	passiveMax        int
	timeSensitiveFrom int
	criticalFrom      int
}

func defaultLevels() effectiveLevels {
	return effectiveLevels{passiveMax: 0, timeSensitiveFrom: 8, criticalFrom: 10}
}

// merge applies the non-nil values of lc on top of base.
func (lc *LevelConfig) merge(base effectiveLevels) effectiveLevels {
	if lc == nil {
		return base
	}
	if lc.PassiveMax != nil {
		base.passiveMax = *lc.PassiveMax
	}
	if lc.TimeSensitiveFrom != nil {
		base.timeSensitiveFrom = *lc.TimeSensitiveFrom
	}
	if lc.CriticalFrom != nil {
		base.criticalFrom = *lc.CriticalFrom
	}
	return base
}

func (e effectiveLevels) validate(scope string) error {
	if e.passiveMax >= e.timeSensitiveFrom {
		return fmt.Errorf("config: %s: passive_max (%d) must be lower than timesensitive_from (%d)", scope, e.passiveMax, e.timeSensitiveFrom)
	}
	if e.timeSensitiveFrom > e.criticalFrom {
		return fmt.Errorf("config: %s: timesensitive_from (%d) must not be higher than critical_from (%d)", scope, e.timeSensitiveFrom, e.criticalFrom)
	}
	return nil
}

func intPtr(i int) *int    { return &i }
func boolPtr(b bool) *bool { return &b }

// groupFromApp reports whether the app name should be used as Bark group (default true).
func (c *Config) groupFromApp() bool {
	return c.GroupFromApp == nil || *c.GroupFromApp
}

// DefaultConfig implements plugin.Configurer.
func (c *BarkForwardPlugin) DefaultConfig() any {
	return &Config{
		GotifyHost:        defaultGotifyHost,
		GotifyClientToken: "",
		BarkURL:           defaultBarkURL,
		ReconnectDelay:    10,
		MaxPayload:        defaultMaxPayload,
		Levels: LevelConfig{
			PassiveMax:        intPtr(0),
			TimeSensitiveFrom: intPtr(8),
			CriticalFrom:      intPtr(10),
		},
		GroupFromApp: boolPtr(true),
		Recipients: []Recipient{
			{
				Name:        "default",
				DeviceKey:   "",
				MinPriority: 0,
				MaxLevel:    "",
			},
		},
	}
}

// isUnconfigured reports whether the config is still the untouched default: no client token
// and no device key anywhere. Gotify validates the stored config on every start; rejecting the
// pristine default would make Gotify treat it as "outdated" and prepend a warning block to the
// config on every restart. So the default is accepted as "not configured yet" and Enable()
// refuses to start until the user fills in token and device key.
func (c *Config) isUnconfigured() bool {
	if c.GotifyClientToken != "" || c.BarkDeviceKey != "" {
		return false
	}
	for _, r := range c.Recipients {
		if r.DeviceKey != "" {
			return false
		}
	}
	return true
}

// errNotConfigured is returned by Enable() while the config is still the untouched default.
var errNotConfigured = errors.New("plugin is not configured yet: set gotify_client_token and at least one recipient device_key in the Configurer, then enable the plugin again")

// ValidateAndSetConfig implements plugin.Configurer.
func (c *BarkForwardPlugin) ValidateAndSetConfig(config any) error {
	newConfig := config.(*Config)

	if newConfig.isUnconfigured() {
		c.config = nil
		logf("configuration is still the default (no token, no device key); waiting for setup.")
		return nil
	}

	if newConfig.GotifyHost == "" {
		return errors.New("config: GotifyHost cannot be empty")
	}
	if newConfig.GotifyClientToken == "" {
		return errors.New("config: GotifyClientToken cannot be empty. Create a client in Gotify for this plugin")
	}
	if newConfig.BarkURL == "" {
		return errors.New("config: BarkURL cannot be empty")
	}
	if newConfig.ReconnectDelay <= 0 {
		newConfig.ReconnectDelay = 10
	}
	if newConfig.MaxPayload <= 0 {
		newConfig.MaxPayload = defaultMaxPayload
	}
	if newConfig.MaxPayload < minMaxPayload {
		return fmt.Errorf("config: max_payload %d is too small: device_key, title, level, volume and group alone need ~200 bytes, so the body would always be dropped; use at least %d (default %d)", newConfig.MaxPayload, minMaxPayload, defaultMaxPayload)
	}
	if newConfig.MaxPayload > apnsPayloadMaximum {
		return fmt.Errorf("config: max_payload %d exceeds Apple's APNs limit of %d bytes", newConfig.MaxPayload, apnsPayloadMaximum)
	}
	if len(newConfig.truncateMarker()) >= newConfig.MaxPayload/2 {
		return fmt.Errorf("config: truncate_marker is too long for max_payload %d", newConfig.MaxPayload)
	}
	if newConfig.GotifyHTTPURL == "" {
		newConfig.GotifyHTTPURL = httpURLFromWS(newConfig.GotifyHost)
	}

	// Backwards compatibility: the legacy single device key becomes the recipient "default".
	// Gotify unmarshals the stored YAML on top of DefaultConfig(), so an old config still
	// carries the empty template recipient "default" from the defaults - fill that one.
	if newConfig.BarkDeviceKey != "" {
		legacy := Recipient{
			Name:          "default",
			DeviceKey:     newConfig.BarkDeviceKey,
			EncryptionKey: newConfig.EncryptionKey,
			EncryptionIV:  newConfig.EncryptionIV,
		}
		replaced := false
		for i := range newConfig.Recipients {
			r := &newConfig.Recipients[i]
			if r.Name == "default" && r.DeviceKey == "" {
				*r = legacy
				replaced = true
				break
			}
		}
		if !replaced {
			newConfig.Recipients = append(newConfig.Recipients, legacy)
		}
	}
	if len(newConfig.Recipients) == 0 {
		return errors.New("config: no recipients configured and bark_device_key is empty")
	}

	globalLevels := newConfig.Levels.merge(defaultLevels())
	if err := globalLevels.validate("levels"); err != nil {
		return err
	}

	seen := map[string]struct{}{}
	for i := range newConfig.Recipients {
		r := &newConfig.Recipients[i]
		if r.Name == "" {
			r.Name = fmt.Sprintf("recipient-%d", i+1)
		}
		if _, dup := seen[r.Name]; dup {
			return fmt.Errorf("config: recipient name %q is used more than once", r.Name)
		}
		seen[r.Name] = struct{}{}
		if r.DeviceKey == "" {
			return fmt.Errorf("config: recipient %q: device_key cannot be empty", r.Name)
		}
		if r.MaxLevel != "" {
			if _, ok := levelRank[r.MaxLevel]; !ok {
				return fmt.Errorf("config: recipient %q: max_level %q is invalid (passive, active, timeSensitive, critical)", r.Name, r.MaxLevel)
			}
		}
		if (r.EncryptionKey == "") != (r.EncryptionIV == "") {
			return fmt.Errorf("config: recipient %q: encryption_key and encryption_iv must both be set or both be empty", r.Name)
		}
		if r.EncryptionKey != "" {
			if l := len(r.EncryptionKey); l != 16 && l != 32 {
				return fmt.Errorf("config: recipient %q: encryption_key must be 16 or 32 bytes, got %d", r.Name, l)
			}
			if l := len(r.EncryptionIV); l != aes.BlockSize {
				return fmt.Errorf("config: recipient %q: encryption_iv must be %d bytes, got %d", r.Name, aes.BlockSize, l)
			}
		}
		r.levels = r.Levels.merge(globalLevels)
		if err := r.levels.validate("recipient " + r.Name); err != nil {
			return err
		}
		r.apps = nil
		if len(r.Apps) > 0 {
			r.apps = make(map[string]struct{}, len(r.Apps))
			for _, a := range r.Apps {
				r.apps[strings.ToLower(strings.TrimSpace(a))] = struct{}{}
			}
		}
	}

	c.config = newConfig
	logf("configuration updated and validated (%d recipient(s)).", len(newConfig.Recipients))
	return nil
}

// httpURLFromWS turns ws://host -> http://host and wss://host -> https://host.
func httpURLFromWS(wsURL string) string {
	u := strings.TrimSpace(wsURL)
	u = strings.TrimRight(u, "/")
	switch {
	case strings.HasPrefix(u, "wss://"):
		return "https://" + strings.TrimPrefix(u, "wss://")
	case strings.HasPrefix(u, "ws://"):
		return "http://" + strings.TrimPrefix(u, "ws://")
	}
	return u
}
