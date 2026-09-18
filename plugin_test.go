package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotify/plugin-api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestAPICompatibility(t *testing.T) {
	assert.Implements(t, (*plugin.Plugin)(nil), new(BarkForwardPlugin))
	assert.Implements(t, (*plugin.Configurer)(nil), new(BarkForwardPlugin))
	assert.Implements(t, (*plugin.Displayer)(nil), new(BarkForwardPlugin))
}

func TestDefaultConfig(t *testing.T) {
	p := &BarkForwardPlugin{}
	cfg := p.DefaultConfig().(*Config)

	assert.Equal(t, "ws://localhost:80", cfg.GotifyHost)
	assert.Equal(t, "", cfg.GotifyClientToken)
	assert.Equal(t, "https://api.day.app/push", cfg.BarkURL)
	assert.Equal(t, 10, cfg.ReconnectDelay)
	assert.True(t, *cfg.GroupFromApp)
	assert.True(t, cfg.groupFromApp())
	require.Len(t, cfg.Recipients, 1)
	assert.Equal(t, "default", cfg.Recipients[0].Name)

	// The default config must survive a YAML round trip (that is what the Gotify UI does).
	out, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	var back Config
	require.NoError(t, yaml.Unmarshal(out, &back))
	assert.Equal(t, cfg.GotifyHost, back.GotifyHost)
	assert.Equal(t, 8, *back.Levels.TimeSensitiveFrom)
	assert.Equal(t, 10, *back.Levels.CriticalFrom)
}

func validConfig() *Config {
	return &Config{
		GotifyHost:        "ws://localhost",
		GotifyClientToken: "token",
		BarkURL:           "https://api.day.app/push",
		Recipients:        []Recipient{{Name: "a", DeviceKey: "key"}},
	}
}

func TestValidateAndSetConfig(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"empty GotifyHost", func(c *Config) { c.GotifyHost = "" }, "GotifyHost cannot be empty"},
		{"empty GotifyClientToken", func(c *Config) { c.GotifyClientToken = "" }, "GotifyClientToken cannot be empty"},
		{"empty BarkURL", func(c *Config) { c.BarkURL = "" }, "BarkURL cannot be empty"},
		{"no recipients and no legacy key", func(c *Config) { c.Recipients = nil }, "no recipients configured"},
		{"recipient without device key", func(c *Config) { c.Recipients[0].DeviceKey = "" }, "device_key cannot be empty"},
		{"invalid max_level", func(c *Config) { c.Recipients[0].MaxLevel = "loud" }, "max_level \"loud\" is invalid"},
		{"duplicate recipient name", func(c *Config) {
			c.Recipients = append(c.Recipients, Recipient{Name: "a", DeviceKey: "k2"})
		}, "used more than once"},
		{"key without iv", func(c *Config) { c.Recipients[0].EncryptionKey = "1234567890123456" }, "must both be set"},
		{"bad key length", func(c *Config) {
			c.Recipients[0].EncryptionKey = "short"
			c.Recipients[0].EncryptionIV = "1234567890123456"
		}, "encryption_key must be 16 or 32 bytes"},
		{"bad iv length", func(c *Config) {
			c.Recipients[0].EncryptionKey = "1234567890123456"
			c.Recipients[0].EncryptionIV = "short"
		}, "encryption_iv must be 16 bytes"},
		{"passive_max >= timesensitive_from", func(c *Config) {
			c.Levels = LevelConfig{PassiveMax: intPtr(8), TimeSensitiveFrom: intPtr(8)}
		}, "passive_max (8) must be lower"},
		{"timesensitive_from > critical_from", func(c *Config) {
			c.Levels = LevelConfig{TimeSensitiveFrom: intPtr(12), CriticalFrom: intPtr(10)}
		}, "timesensitive_from (12) must not be higher"},
		{"recipient level override invalid", func(c *Config) {
			c.Recipients[0].Levels = &LevelConfig{CriticalFrom: intPtr(3)}
		}, "recipient a: timesensitive_from (8) must not be higher than critical_from (3)"},
		{"valid", func(c *Config) {}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(cfg)
			p := &BarkForwardPlugin{}
			err := p.ValidateAndSetConfig(cfg)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
				assert.NotNil(t, p.config)
			}
		})
	}
}

func TestValidateDerivedValues(t *testing.T) {
	p := &BarkForwardPlugin{}
	cfg := validConfig()
	cfg.GotifyHost = "wss://gotify.example.ch/"
	cfg.ReconnectDelay = 0
	cfg.Recipients[0].Name = ""
	cfg.Recipients[0].Apps = []string{" Uptime-Kuma ", "7"}
	require.NoError(t, p.ValidateAndSetConfig(cfg))

	assert.Equal(t, 10, p.config.ReconnectDelay)
	assert.Equal(t, "https://gotify.example.ch", p.config.GotifyHTTPURL)
	assert.Equal(t, "recipient-1", p.config.Recipients[0].Name)
	assert.Equal(t, defaultLevels(), p.config.Recipients[0].levels)
	_, ok := p.config.Recipients[0].apps["uptime-kuma"]
	assert.True(t, ok)
	_, ok = p.config.Recipients[0].apps["7"]
	assert.True(t, ok)
}

func TestLegacyConfigBecomesRecipient(t *testing.T) {
	p := &BarkForwardPlugin{}
	cfg := &Config{
		GotifyHost:        "ws://localhost",
		GotifyClientToken: "token",
		BarkURL:           "url",
		BarkDeviceKey:     "legacy-key",
		EncryptionKey:     "1234567890123456",
		EncryptionIV:      "1234567890123456",
	}
	require.NoError(t, p.ValidateAndSetConfig(cfg))
	require.Len(t, p.config.Recipients, 1)
	r := p.config.Recipients[0]
	assert.Equal(t, "default", r.Name)
	assert.Equal(t, "legacy-key", r.DeviceKey)
	assert.Equal(t, "1234567890123456", r.EncryptionKey)
	assert.Equal(t, 0, r.MinPriority)
	assert.Equal(t, "", r.MaxLevel)
}

// TestLegacyYAMLOnTopOfDefaults mirrors what Gotify does: the stored YAML is unmarshalled
// into DefaultConfig(), so keys missing in the old config keep their defaults.
func TestLegacyYAMLOnTopOfDefaults(t *testing.T) {
	legacyYAML := `
gotify_host: ws://localhost:80
gotify_client_token: token
bark_device_key: legacy-key
bark_url: https://api.day.app/push
reconnect_delay: 10
`
	p := &BarkForwardPlugin{}
	cfg := p.DefaultConfig()
	require.NoError(t, yaml.Unmarshal([]byte(legacyYAML), cfg))
	require.NoError(t, p.ValidateAndSetConfig(cfg))

	require.Len(t, p.config.Recipients, 1)
	assert.Equal(t, "default", p.config.Recipients[0].Name)
	assert.Equal(t, "legacy-key", p.config.Recipients[0].DeviceKey)
	assert.Equal(t, defaultLevels(), p.config.Recipients[0].levels)
	assert.True(t, p.config.groupFromApp())
	assert.Equal(t, "http://localhost:80", p.config.GotifyHTTPURL)

	// a fresh default config (empty device key) is rejected with a clear message
	p2 := &BarkForwardPlugin{}
	def := p2.DefaultConfig().(*Config)
	def.GotifyClientToken = "token"
	err := p2.ValidateAndSetConfig(def)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `recipient "default": device_key cannot be empty`)

	// new-style YAML with recipients replaces the template recipient entirely
	newYAML := `
gotify_host: ws://localhost:80
gotify_client_token: token
bark_url: https://api.day.app/push
levels:
  critical_from: 12
group_from_app: false
recipients:
  - name: benj
    device_key: k1
  - name: partner
    device_key: k2
    max_level: timeSensitive
    apps: [Alarm]
    params:
      sound: minuet
`
	p3 := &BarkForwardPlugin{}
	cfg3 := p3.DefaultConfig()
	require.NoError(t, yaml.Unmarshal([]byte(newYAML), cfg3))
	require.NoError(t, p3.ValidateAndSetConfig(cfg3))
	require.Len(t, p3.config.Recipients, 2)
	assert.Equal(t, effectiveLevels{passiveMax: 0, timeSensitiveFrom: 8, criticalFrom: 12}, p3.config.Recipients[0].levels)
	assert.False(t, p3.config.groupFromApp())
	assert.Equal(t, "minuet", p3.config.Recipients[1].Params["sound"])
	assert.Equal(t, LevelTimeSensitive, p3.config.Recipients[1].MaxLevel)
}

func TestEnableWithoutConfig(t *testing.T) {
	p := &BarkForwardPlugin{}
	err := p.Enable()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not configured")
}

func TestMapPriority(t *testing.T) {
	lv := defaultLevels()
	tests := []struct {
		priority int
		level    string
		volume   int
	}{
		{-1, LevelPassive, 0},
		{0, LevelPassive, 0},
		{1, LevelActive, 0},
		{5, LevelActive, 0},
		{7, LevelActive, 0},
		{8, LevelTimeSensitive, 0},
		{9, LevelTimeSensitive, 0},
		{10, LevelCritical, 1},
		{11, LevelCritical, 2},
		{15, LevelCritical, 6},
		{19, LevelCritical, 10},
		{20, LevelCritical, 10},
		{99, LevelCritical, 10},
	}
	for _, tt := range tests {
		level, volume := mapPriority(tt.priority, lv)
		assert.Equal(t, tt.level, level, "priority %d", tt.priority)
		assert.Equal(t, tt.volume, volume, "priority %d", tt.priority)
	}

	// custom thresholds
	custom := effectiveLevels{passiveMax: -1, timeSensitiveFrom: 5, criticalFrom: 12}
	level, _ := mapPriority(0, custom)
	assert.Equal(t, LevelActive, level)
	level, _ = mapPriority(5, custom)
	assert.Equal(t, LevelTimeSensitive, level)
	level, volume := mapPriority(12, custom)
	assert.Equal(t, LevelCritical, level)
	assert.Equal(t, 1, volume)
}

func TestCapLevel(t *testing.T) {
	assert.Equal(t, LevelCritical, capLevel(LevelCritical, ""))
	assert.Equal(t, LevelTimeSensitive, capLevel(LevelCritical, LevelTimeSensitive))
	assert.Equal(t, LevelActive, capLevel(LevelActive, LevelTimeSensitive))
	assert.Equal(t, LevelPassive, capLevel(LevelPassive, LevelActive))
	assert.Equal(t, LevelPassive, capLevel(LevelCritical, LevelPassive))
}

// configuredPlugin returns a plugin with a validated config and a pre-filled app cache.
func configuredPlugin(t *testing.T, cfg *Config) *BarkForwardPlugin {
	t.Helper()
	p := &BarkForwardPlugin{}
	require.NoError(t, p.ValidateAndSetConfig(cfg))
	p.apps = appCache{names: map[uint]string{1: "Uptime-Kuma", 2: "Backup"}, lastRefresh: time.Now()}
	return p
}

func TestBuildBarkRequestMapping(t *testing.T) {
	cfg := validConfig()
	cfg.Recipients[0].Params = map[string]any{"sound": "minuet", "icon": "https://x/y.png"}
	p := configuredPlugin(t, cfg)
	r := &p.config.Recipients[0]

	// normal message
	req := p.buildBarkRequest(r, streamMessage{ID: 1, AppID: 2, Title: "T", Message: "M", Priority: 4}, "Backup")
	assert.Equal(t, "key", req["device_key"])
	assert.Equal(t, "T", req["title"])
	assert.Equal(t, "M", req["body"])
	assert.Equal(t, LevelActive, req["level"])
	assert.NotContains(t, req, "volume")
	assert.Equal(t, "Backup", req["group"])
	assert.Equal(t, "minuet", req["sound"])
	assert.Equal(t, "https://x/y.png", req["icon"])

	// critical with volume
	req = p.buildBarkRequest(r, streamMessage{AppID: 2, Priority: 13}, "Backup")
	assert.Equal(t, LevelCritical, req["level"])
	assert.Equal(t, 4, req["volume"])

	// time sensitive
	req = p.buildBarkRequest(r, streamMessage{AppID: 2, Priority: 8}, "Backup")
	assert.Equal(t, LevelTimeSensitive, req["level"])

	// passive
	req = p.buildBarkRequest(r, streamMessage{AppID: 2, Priority: 0}, "Backup")
	assert.Equal(t, LevelPassive, req["level"])
}

func TestBuildBarkRequestExtrasWinAndCapApplies(t *testing.T) {
	cfg := validConfig()
	cfg.Recipients = []Recipient{
		{Name: "full", DeviceKey: "k1"},
		{Name: "capped", DeviceKey: "k2", MaxLevel: LevelTimeSensitive},
	}
	p := configuredPlugin(t, cfg)

	msg := streamMessage{AppID: 1, Priority: 3, Extras: map[string]any{
		extrasKey: map[string]any{
			"level":       LevelCritical,
			"volume":      7,
			"group":       "Custom",
			"sound":       "alarm",
			"device_key":  "hijack",
			"device_keys": []any{"a", "b"},
		},
	}}

	// extras override the mapping for the uncapped recipient
	req := p.buildBarkRequest(&p.config.Recipients[0], msg, "Uptime-Kuma")
	assert.Equal(t, LevelCritical, req["level"])
	assert.Equal(t, 7, req["volume"])
	assert.Equal(t, "Custom", req["group"])
	assert.Equal(t, "alarm", req["sound"])
	assert.Equal(t, "k1", req["device_key"], "device key must not be overridable from a message")
	assert.NotContains(t, req, "device_keys")

	// the cap also applies to an explicit extras level, and volume is dropped
	req = p.buildBarkRequest(&p.config.Recipients[1], msg, "Uptime-Kuma")
	assert.Equal(t, LevelTimeSensitive, req["level"])
	assert.NotContains(t, req, "volume")
	assert.Equal(t, "k2", req["device_key"])
}

func TestBuildBarkRequestRecipientLevels(t *testing.T) {
	cfg := validConfig()
	cfg.Recipients[0].Levels = &LevelConfig{CriticalFrom: intPtr(15), TimeSensitiveFrom: intPtr(10)}
	p := configuredPlugin(t, cfg)
	r := &p.config.Recipients[0]

	req := p.buildBarkRequest(r, streamMessage{Priority: 10}, "")
	assert.Equal(t, LevelTimeSensitive, req["level"])
	req = p.buildBarkRequest(r, streamMessage{Priority: 16}, "")
	assert.Equal(t, LevelCritical, req["level"])
	assert.Equal(t, 2, req["volume"])
}

func TestResolveGroup(t *testing.T) {
	cfg := validConfig()
	cfg.Groups = map[string]string{"Uptime-Kuma": "Monitoring", "2": "ById"}
	cfg.Recipients = []Recipient{
		{Name: "auto", DeviceKey: "k1"},
		{Name: "fixed", DeviceKey: "k2", Group: "Fix"},
	}
	p := configuredPlugin(t, cfg)
	auto, fixed := &p.config.Recipients[0], &p.config.Recipients[1]

	assert.Equal(t, "Monitoring", p.resolveGroup(auto, streamMessage{AppID: 1}, "Uptime-Kuma"))
	assert.Equal(t, "ById", p.resolveGroup(auto, streamMessage{AppID: 2}, "Backup"))
	assert.Equal(t, "Other", p.resolveGroup(auto, streamMessage{AppID: 3}, "Other"))
	assert.Equal(t, "", p.resolveGroup(auto, streamMessage{AppID: 9}, ""), "unknown app -> no group")
	assert.Equal(t, "Fix", p.resolveGroup(fixed, streamMessage{AppID: 1}, "Uptime-Kuma"))

	p.config.GroupFromApp = boolPtr(false)
	assert.Equal(t, "", p.resolveGroup(auto, streamMessage{AppID: 3}, "Other"))
	assert.Equal(t, "Monitoring", p.resolveGroup(auto, streamMessage{AppID: 1}, "Uptime-Kuma"), "groups map still applies")
}

func TestWantsMessage(t *testing.T) {
	cfg := validConfig()
	cfg.Recipients = []Recipient{
		{Name: "all", DeviceKey: "k1"},
		{Name: "filtered", DeviceKey: "k2", MinPriority: 5, Apps: []string{"uptime-kuma", "7"}},
	}
	p := configuredPlugin(t, cfg)
	all, filtered := &p.config.Recipients[0], &p.config.Recipients[1]

	ok, _ := all.wantsMessage(streamMessage{AppID: 3, Priority: 0}, "Any")
	assert.True(t, ok)

	ok, reason := filtered.wantsMessage(streamMessage{AppID: 1, Priority: 4}, "Uptime-Kuma")
	assert.False(t, ok)
	assert.Contains(t, reason, "below min_priority")

	ok, _ = filtered.wantsMessage(streamMessage{AppID: 1, Priority: 5}, "Uptime-Kuma")
	assert.True(t, ok, "app name match is case-insensitive")

	ok, _ = filtered.wantsMessage(streamMessage{AppID: 7, Priority: 5}, "")
	assert.True(t, ok, "match by app id even when the name is unknown")

	ok, reason = filtered.wantsMessage(streamMessage{AppID: 2, Priority: 9}, "Backup")
	assert.False(t, ok)
	assert.Contains(t, reason, "not in apps filter")
}

func TestForwardToBarkEndToEnd(t *testing.T) {
	// fake Bark server capturing plain requests, and fake Gotify serving /application
	type captured struct {
		path string
		body map[string]any
	}
	var got []captured
	bark := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, captured{r.URL.Path, body})
		w.WriteHeader(http.StatusOK)
	}))
	defer bark.Close()

	var appCalls int32
	gotify := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/application", r.URL.Path)
		assert.Equal(t, "token", r.Header.Get("X-Gotify-Key"))
		atomic.AddInt32(&appCalls, 1)
		_, _ = w.Write([]byte(`[{"id":1,"name":"Uptime-Kuma"},{"id":2,"name":"Backup"}]`))
	}))
	defer gotify.Close()

	cfg := &Config{
		GotifyHost:        "ws://ignored",
		GotifyHTTPURL:     gotify.URL,
		GotifyClientToken: "token",
		BarkURL:           bark.URL + "/push",
		Recipients: []Recipient{
			{Name: "benj", DeviceKey: "benj-key"},
			{Name: "partner", DeviceKey: "partner-key", MinPriority: 5, MaxLevel: LevelTimeSensitive},
			{Name: "enc", DeviceKey: "enc-key", EncryptionKey: "1234567890123456", EncryptionIV: "1234567890123456"},
		},
	}
	p := &BarkForwardPlugin{}
	require.NoError(t, p.ValidateAndSetConfig(cfg))
	p.httpClient = &http.Client{Timeout: 5 * time.Second}

	// priority 12 -> critical volume 3 for benj, timeSensitive for partner, encrypted for enc
	p.forwardToBark(streamMessage{ID: 1, AppID: 1, Title: "Down", Message: "host down", Priority: 12})
	// priority 2 -> only benj and enc (partner min_priority 5)
	p.forwardToBark(streamMessage{ID: 2, AppID: 2, Title: "Info", Message: "ok", Priority: 2})

	require.Len(t, got, 5)
	assert.Equal(t, int32(1), atomic.LoadInt32(&appCalls), "application list is fetched once and cached")

	assert.Equal(t, "/push", got[0].path)
	assert.Equal(t, "benj-key", got[0].body["device_key"])
	assert.Equal(t, LevelCritical, got[0].body["level"])
	assert.Equal(t, float64(3), got[0].body["volume"])
	assert.Equal(t, "Uptime-Kuma", got[0].body["group"])

	assert.Equal(t, "partner-key", got[1].body["device_key"])
	assert.Equal(t, LevelTimeSensitive, got[1].body["level"])
	assert.NotContains(t, got[1].body, "volume")

	assert.Equal(t, "/enc-key", got[2].path, "encrypted push goes to /<device_key>")
	assert.Contains(t, got[2].body, "ciphertext")
	assert.NotContains(t, got[2].body, "title")

	assert.Equal(t, "benj-key", got[3].body["device_key"])
	assert.Equal(t, LevelActive, got[3].body["level"])
	assert.Equal(t, "Backup", got[3].body["group"])
	assert.Equal(t, "/enc-key", got[4].path)
}

func TestAppNameRefreshIsRateLimited(t *testing.T) {
	var calls int32
	gotify := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`[{"id":1,"name":"A"}]`))
	}))
	defer gotify.Close()

	cfg := validConfig()
	cfg.GotifyHTTPURL = gotify.URL
	p := &BarkForwardPlugin{}
	require.NoError(t, p.ValidateAndSetConfig(cfg))
	p.httpClient = &http.Client{Timeout: 5 * time.Second}

	assert.Equal(t, "A", p.appName(1))
	assert.Equal(t, "", p.appName(99), "unknown id right after a refresh does not hit the API again")
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls))

	p.apps.lastRefresh = time.Now().Add(-appRefreshMinInterval)
	assert.Equal(t, "", p.appName(99))
	assert.Equal(t, int32(2), atomic.LoadInt32(&calls), "after the interval the list is fetched again")
}

func TestEncryptAESCBC(t *testing.T) {
	tests := []struct {
		name      string
		key       string
		iv        string
		plaintext string
		wantErr   string
	}{
		{"valid AES-128", "1234567890123456", "1234567890123456", `{"title":"test","body":"hello"}`, ""},
		{"valid AES-256", "12345678901234567890123456789012", "1234567890123456", `{"title":"test","body":"hello"}`, ""},
		{"invalid key length", "shortkey", "1234567890123456", `{"title":"test"}`, "encryption key must be 16 or 32 bytes"},
		{"invalid IV length", "1234567890123456", "short", `{"title":"test"}`, "encryption IV must be 16 bytes"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := encryptAESCBC(tt.key, tt.iv, []byte(tt.plaintext))
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
				assert.NotEmpty(t, result)
				assert.Regexp(t, `^[A-Za-z0-9+/]+=*$`, result)
			}
		})
	}
}

func TestPushURLs(t *testing.T) {
	assert.Equal(t, "https://api.day.app/push", plainPushURL(""))
	assert.Equal(t, "https://api.day.app/push", plainPushURL("https://api.day.app"))
	assert.Equal(t, "https://api.day.app/push", plainPushURL("https://api.day.app/push/"))
	assert.Equal(t, "https://api.day.app/abc", encryptedPushURL("https://api.day.app/push", "abc"))
	assert.Equal(t, "https://bark.example.ch/abc", encryptedPushURL("https://bark.example.ch/", "abc"))
}

func TestHTTPURLFromWS(t *testing.T) {
	assert.Equal(t, "http://localhost:80", httpURLFromWS("ws://localhost:80"))
	assert.Equal(t, "https://g.example.ch", httpURLFromWS("wss://g.example.ch/"))
	assert.Equal(t, "https://already", httpURLFromWS("https://already"))
}
