package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEncryptedSizeMatchesRealCiphertext(t *testing.T) {
	// The estimate must equal the real base64 length for every padding case.
	for n := 0; n < 100; n++ {
		plain := []byte(strings.Repeat("x", n))
		ct, err := encryptAESCBC("1234567890123456", "1234567890123456", plain)
		require.NoError(t, err)
		assert.Equal(t, len(ct), encryptedSize(n), "plaintext length %d", n)
	}
	for _, n := range []int{2700, 2703, 2704, 2705, 3000, 4096} {
		plain := []byte(strings.Repeat("y", n))
		ct, err := encryptAESCBC("12345678901234567890123456789012", "1234567890123456", plain)
		require.NoError(t, err)
		assert.Equal(t, len(ct), encryptedSize(n), "plaintext length %d", n)
	}
}

func TestCutUTF8(t *testing.T) {
	s := "Grüezi wohl" // ü = 2 bytes at index 2..3
	assert.Equal(t, "Gr", cutUTF8(s, 3), "must not split the ü")
	assert.Equal(t, "Grü", cutUTF8(s, 4))
	assert.Equal(t, s, cutUTF8(s, 100))
	assert.Equal(t, "", cutUTF8(s, 0))
	assert.True(t, utf8.ValidString(cutUTF8("äöüäöüäöü", 7)))
}

func baseRequest(body string) map[string]any {
	return map[string]any{
		"device_key": "abcdefghijklmnopqrstuv",
		"title":      "Server down",
		"body":       body,
		"level":      LevelCritical,
		"volume":     7,
		"group":      "Uptime-Kuma",
	}
}

func TestFitPayloadLeavesShortMessagesAlone(t *testing.T) {
	req := baseRequest("kurz")
	before, after := fitPayload(req, false, defaultMaxPayload, defaultTruncateMarker)
	assert.Equal(t, 4, before)
	assert.Equal(t, 4, after)
	assert.Equal(t, "kurz", req["body"])

	req = baseRequest("kurz")
	_, after = fitPayload(req, true, defaultMaxPayload, defaultTruncateMarker)
	assert.Equal(t, 4, after)
}

func TestFitPayloadPlain(t *testing.T) {
	long := strings.Repeat("Zeile mit Log-Ausgabe 0123456789\n", 200) // ~6.6 KB
	req := baseRequest(long)
	before, after := fitPayload(req, false, defaultMaxPayload, defaultTruncateMarker)

	assert.Equal(t, len(long), before)
	assert.Less(t, after, before)
	body := req["body"].(string)
	assert.True(t, strings.HasSuffix(body, defaultTruncateMarker))
	assert.True(t, strings.HasPrefix(long, strings.TrimSuffix(body, defaultTruncateMarker)), "body is a prefix of the original")

	data, err := json.Marshal(req)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(data), defaultMaxPayload)
	assert.Greater(t, len(data), defaultMaxPayload-200, "should not cut much more than necessary")

	// everything else untouched
	assert.Equal(t, "Server down", req["title"])
	assert.Equal(t, LevelCritical, req["level"])
	assert.Equal(t, 7, req["volume"])
	assert.Equal(t, "Uptime-Kuma", req["group"])
	assert.Equal(t, "abcdefghijklmnopqrstuv", req["device_key"])
}

func TestFitPayloadEncryptedUsesBase64Size(t *testing.T) {
	// ~3000 bytes fits plain (3000 + ~120 overhead < 3800) but not encrypted (base64 ~4200).
	long := strings.Repeat("abcdefghij", 300)

	plain := baseRequest(long)
	_, after := fitPayload(plain, false, defaultMaxPayload, defaultTruncateMarker)
	assert.Equal(t, len(long), after, "fits unencrypted, nothing to cut")

	enc := baseRequest(long)
	before, after := fitPayload(enc, true, defaultMaxPayload, defaultTruncateMarker)
	assert.Equal(t, len(long), before)
	assert.Less(t, after, before, "must be cut when encrypted")

	data, err := json.Marshal(enc)
	require.NoError(t, err)
	ct, err := encryptAESCBC("1234567890123456", "1234567890123456", data)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(ct), defaultMaxPayload, "real ciphertext must fit the budget")
	assert.Greater(t, len(ct), defaultMaxPayload-200, "should not cut much more than necessary")
}

func TestFitPayloadUmlautsAndEscaping(t *testing.T) {
	// multi-byte characters and JSON-escaped characters ("<", quotes, newlines) inflate the JSON
	long := strings.Repeat("Grüezi \"Welt\" <ä/ö/ü>\n", 300)
	req := baseRequest(long)
	fitPayload(req, false, 1000, "…")

	body := req["body"].(string)
	assert.True(t, utf8.ValidString(body), "cut must not produce invalid UTF-8")
	assert.True(t, strings.HasSuffix(body, "…"))
	data, err := json.Marshal(req)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(data), 1000)
	assert.Greater(t, len(data), 700)
}

func TestFitPayloadEmptyMarker(t *testing.T) {
	req := baseRequest(strings.Repeat("x", 5000))
	fitPayload(req, false, 1000, "")
	body := req["body"].(string)
	assert.False(t, strings.Contains(body, "…"))
	data, _ := json.Marshal(req)
	assert.LessOrEqual(t, len(data), 1000)
}

func TestFitPayloadTinyBudgetDropsBody(t *testing.T) {
	// budget smaller than title + params: the body goes, the alert itself stays
	req := baseRequest(strings.Repeat("x", 500))
	fitPayload(req, false, 100, defaultTruncateMarker)
	assert.Equal(t, "", req["body"])
	assert.Equal(t, LevelCritical, req["level"])
	assert.Equal(t, 7, req["volume"])
}

func TestForwardTruncatesBeforeSending(t *testing.T) {
	// end to end through forwardToBark with the default config
	cfg := validConfig()
	cfg.Recipients = []Recipient{
		{Name: "plain", DeviceKey: "k1"},
		{Name: "enc", DeviceKey: "k2", EncryptionKey: "1234567890123456", EncryptionIV: "1234567890123456"},
	}
	p := configuredPlugin(t, cfg)

	var sizes []int
	p.httpClient = newRecordingClient(func(path string, body []byte) {
		sizes = append(sizes, len(body))
	})

	p.forwardToBark(streamMessage{ID: 1, AppID: 1, Title: "T", Message: strings.Repeat("lang ", 2000), Priority: 12})
	require.Len(t, sizes, 2)
	assert.LessOrEqual(t, sizes[0], defaultMaxPayload, "plain JSON within budget")
	// encrypted request body is {"ciphertext":"<base64>"}: base64 within budget plus wrapper
	assert.LessOrEqual(t, sizes[1], defaultMaxPayload+len(`{"ciphertext":""}`))
}

// recordingTransport answers every request with 200 and hands path and body to fn.
type recordingTransport struct {
	fn func(path string, body []byte)
}

func (rt recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	rt.fn(req.URL.Path, body)
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Body:       io.NopCloser(strings.NewReader(`{"code":200}`)),
		Header:     http.Header{},
		Request:    req,
	}, nil
}

func newRecordingClient(fn func(path string, body []byte)) *http.Client {
	return &http.Client{Transport: recordingTransport{fn: fn}}
}

func TestMaxPayloadConfig(t *testing.T) {
	p := &BarkForwardPlugin{}
	cfg := validConfig()
	require.NoError(t, p.ValidateAndSetConfig(cfg))
	assert.Equal(t, defaultMaxPayload, p.config.MaxPayload)
	assert.Equal(t, defaultTruncateMarker, p.config.truncateMarker())

	cfg = validConfig()
	cfg.MaxPayload = 2000
	empty := ""
	cfg.TruncateMarker = &empty
	require.NoError(t, p.ValidateAndSetConfig(cfg))
	assert.Equal(t, 2000, p.config.MaxPayload)
	assert.Equal(t, "", p.config.truncateMarker())

	cfg = validConfig()
	cfg.MaxPayload = 40
	err := p.ValidateAndSetConfig(cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "truncate_marker is too long")
}

func TestFitPayloadProperty(t *testing.T) {
	// Many budgets, plain and encrypted, mixed content: always within budget, never wasteful,
	// body always a valid UTF-8 prefix of the original plus marker (or empty).
	unit := "Grüezi \"Welt\" <ä/ö/ü> plain text 0123456789\n"
	long := strings.Repeat(unit, 400)
	for _, encrypted := range []bool{false, true} {
		for budget := 300; budget <= 4096; budget += 37 {
			req := baseRequest(long)
			fitPayload(req, encrypted, budget, defaultTruncateMarker)
			data, err := json.Marshal(req)
			require.NoError(t, err)
			size := payloadSize(len(data), encrypted)
			assert.LessOrEqual(t, size, budget, "encrypted=%v budget=%d", encrypted, budget)
			body := req["body"].(string)
			assert.True(t, utf8.ValidString(body))
			if body != "" {
				assert.True(t, strings.HasSuffix(body, defaultTruncateMarker), "encrypted=%v budget=%d", encrypted, budget)
				assert.True(t, strings.HasPrefix(long, strings.TrimSuffix(body, defaultTruncateMarker)))
				assert.Greater(t, size, budget-250, "wasted budget: encrypted=%v budget=%d size=%d", encrypted, budget, size)
			}
		}
	}
}
