package main

import (
	"crypto/aes"
	"encoding/json"
	"unicode/utf8"
)

// apnsPayloadMaximum is Apple's hard limit for a remote notification payload.
const apnsPayloadMaximum = 4096

// maxTruncateRounds bounds the shrink loop. Each round cuts a proportional estimate of the
// excess; JSON escaping and base64 rounding make the size non-linear in the body length,
// so a few rounds may be needed. The final round guarantees the budget.
const maxTruncateRounds = 20

// encryptedSize returns the base64 length of an AES-CBC/PKCS7 ciphertext for n plaintext bytes.
// Padding always adds 1..16 bytes so the padded length is the next multiple of 16 above n.
func encryptedSize(n int) int {
	padded := (n/aes.BlockSize + 1) * aes.BlockSize
	return (padded + 2) / 3 * 4
}

// payloadSize returns the number of bytes that count against the Bark/APNs budget for the
// given Bark JSON: the JSON itself for plain pushes, the base64 ciphertext for encrypted ones.
func payloadSize(jsonLen int, encrypted bool) int {
	if encrypted {
		return encryptedSize(jsonLen)
	}
	return jsonLen
}

// jsonStringLen returns the number of bytes s occupies inside a JSON document (escaped,
// without the surrounding quotes).
func jsonStringLen(s string) int {
	b, err := json.Marshal(s)
	if err != nil {
		return len(s)
	}
	return len(b) - 2
}

// cutUTF8 shortens s to at most n bytes without splitting a multi-byte character.
func cutUTF8(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// fitPayload shortens req["body"] until the payload fits into budget bytes. It returns the
// original and the final body length in bytes (equal when nothing was cut). Only the body is
// touched; title, level, volume, group and all other parameters stay intact. If even an empty
// body does not fit, the request is left with an empty body so the alert itself gets through.
func fitPayload(req map[string]any, encrypted bool, budget int, marker string) (before, after int) {
	body, _ := req["body"].(string)
	before = len(body)

	data, err := json.Marshal(req)
	if err != nil || payloadSize(len(data), encrypted) <= budget {
		return before, before
	}

	remaining := body
	markerPending := true // the marker is not part of the JSON until the first cut
	for round := 0; round < maxTruncateRounds && remaining != ""; round++ {
		excess := payloadSize(len(data), encrypted) - budget
		if excess <= 0 {
			break
		}
		if markerPending {
			excess += jsonStringLen(marker)
			if encrypted {
				excess += aes.BlockSize // padding/base64 rounding reserve
			}
		}
		if encrypted {
			excess = excess*3/4 + 1 // base64 bytes -> JSON bytes
		}
		// JSON bytes -> raw body bytes: escaping only inflates, so scale by the body's ratio.
		cut := excess
		if bodyJSON := jsonStringLen(remaining); bodyJSON > len(remaining) {
			cut = excess*len(remaining)/bodyJSON + 1
		}
		if round == maxTruncateRounds-1 {
			cut = len(remaining) // last resort: drop the rest
		}
		remaining = cutUTF8(remaining, len(remaining)-cut)
		markerPending = false
		if remaining == "" {
			req["body"] = ""
		} else {
			req["body"] = remaining + marker
		}
		if data, err = json.Marshal(req); err != nil {
			break
		}
	}

	final, _ := req["body"].(string)
	return before, len(final)
}
