package webhooksig

import (
	"errors"
	"testing"
	"time"
)

func TestSignVerify(t *testing.T) {
	secret := []byte("s3cret")
	body := []byte(`{"id":"evt_1","status":"assigned"}`)
	now := time.Unix(1_800_000_000, 0)
	valid := Sign(secret, body, now)

	tests := []struct {
		name   string
		secret []byte
		header string
		body   []byte
		now    time.Time
		want   error
	}{
		{"valid", secret, valid, body, now, nil},
		{"valid within tolerance", secret, valid, body, now.Add(4 * time.Minute), nil},
		{"body changed", secret, valid, []byte(`{"id":"evt_1","status":"delivered"}`), now, ErrInvalid},
		{"other secret", []byte("other"), valid, body, now, ErrInvalid},
		{"missing", secret, "", body, now, ErrMissing},
		{"garbage", secret, "garbage", body, now, ErrInvalid},
		{"no v1", secret, "t=1800000000", body, now, ErrInvalid},
		{"bad hex", secret, "t=1800000000,v1=zz", body, now, ErrInvalid},
		{"6 min old", secret, valid, body, now.Add(6 * time.Minute), ErrExpired},
		{"6 min in the future", secret, valid, body, now.Add(-6 * time.Minute), ErrExpired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Verify(tt.secret, tt.header, tt.body, tt.now, 5*time.Minute)
			if !errors.Is(err, tt.want) {
				t.Errorf("Verify() = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestSignFormat(t *testing.T) {
	got := Sign([]byte("k"), []byte("b"), time.Unix(42, 0))
	if len(got) != len("t=42,v1=")+64 || got[:8] != "t=42,v1=" {
		t.Errorf("Sign() = %q, want t=42,v1=<64 hex chars>", got)
	}
}
