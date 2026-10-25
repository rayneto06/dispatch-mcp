// Package webhooksig signs and verifies webhook bodies. The sender (a fleet
// partner) and the receiver (dispatch-mcp) must agree byte for byte, which is
// why this is the one piece of code both sides share.
//
// The header looks like
//
//	Webhook-Signature: t=1761400000,v1=5257a869...
//
// where v1 is hex(HMAC-SHA256(secret, "<t>.<raw body>")). Signing the
// timestamp together with the body lets the receiver reject old captures.
package webhooksig

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Header is the HTTP header that carries the signature.
const Header = "Webhook-Signature"

var (
	ErrMissing = errors.New("webhook signature missing")
	ErrInvalid = errors.New("webhook signature invalid")
	ErrExpired = errors.New("webhook signature timestamp outside tolerance")
)

// Sign returns the header value for body sent at t.
func Sign(secret, body []byte, t time.Time) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac(secret, ts, body))
}

// Verify checks header against body. The MAC is checked before the
// timestamp so an attacker cannot learn anything from which error comes back.
func Verify(secret []byte, header string, body []byte, now time.Time, tolerance time.Duration) error {
	if header == "" {
		return ErrMissing
	}
	var ts, sig string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(part, "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			sig = v
		}
	}
	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ErrInvalid
	}
	got, err := hex.DecodeString(sig)
	if err != nil || len(got) == 0 {
		return ErrInvalid
	}
	if !hmac.Equal(got, mac(secret, ts, body)) { // constant-time compare
		return ErrInvalid
	}
	if now.Sub(time.Unix(unix, 0)).Abs() > tolerance {
		return ErrExpired
	}
	return nil
}

func mac(secret []byte, ts string, body []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(ts))
	m.Write([]byte("."))
	m.Write(body)
	return m.Sum(nil)
}
