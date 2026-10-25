package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestNewLoggerUsesCloudLoggingKeys(t *testing.T) {
	var buf bytes.Buffer
	NewLogger(&buf, slog.LevelInfo).Warn("hi", "partner", "alpha")

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("log line is not JSON: %v: %q", err, buf.String())
	}
	if line["severity"] != "WARNING" {
		t.Errorf("severity = %v, want WARNING", line["severity"])
	}
	if line["message"] != "hi" {
		t.Errorf("message = %v, want hi", line["message"])
	}
	for _, k := range []string{"level", "msg"} {
		if _, ok := line[k]; ok {
			t.Errorf("unexpected key %q in %v", k, line)
		}
	}
	if line["partner"] != "alpha" {
		t.Errorf("partner = %v, want alpha", line["partner"])
	}
}

func TestNewLoggerRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	NewLogger(&buf, slog.LevelWarn).Info("quiet")
	if buf.Len() != 0 {
		t.Errorf("info logged at warn level: %q", buf.String())
	}
}

func TestParseLevel(t *testing.T) {
	tests := []struct {
		in      string
		want    slog.Level
		wantErr bool
	}{
		{"", slog.LevelInfo, false},
		{"debug", slog.LevelDebug, false},
		{"INFO", slog.LevelInfo, false},
		{"warn", slog.LevelWarn, false},
		{"error", slog.LevelError, false},
		{"loud", 0, true},
	}
	for _, tt := range tests {
		got, err := ParseLevel(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("ParseLevel(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && got != tt.want {
			t.Errorf("ParseLevel(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestRequestIDRoundTrip(t *testing.T) {
	ctx := WithRequestID(context.Background(), "req_x")
	if got := RequestID(ctx); got != "req_x" {
		t.Errorf("RequestID = %q, want req_x", got)
	}
	if got := RequestID(context.Background()); got != "" {
		t.Errorf("RequestID on empty ctx = %q, want empty", got)
	}
}

func TestNewRequestID(t *testing.T) {
	a, b := NewRequestID(), NewRequestID()
	if len(a) != len("req_")+12 || a[:4] != "req_" {
		t.Errorf("NewRequestID = %q, want req_ + 12 hex chars", a)
	}
	if a == b {
		t.Errorf("two request IDs are equal: %q", a)
	}
}
