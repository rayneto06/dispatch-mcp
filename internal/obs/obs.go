// Package obs holds the small observability helpers shared by both binaries:
// a JSON logger in the shape Cloud Logging understands, and a request ID that
// travels in the context from an MCP tool call to every partner request.
package obs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// RequestIDHeader carries the request ID between services.
const RequestIDHeader = "X-Request-ID"

// NewLogger returns a JSON logger. Cloud Logging reads "severity" and
// "message", so the default "level" and "msg" keys are renamed.
func NewLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) > 0 {
				return a
			}
			switch a.Key {
			case slog.LevelKey:
				// Only the record's own level is a slog.Level; an attribute a
				// caller named "level" (the MCP SDK logs one) passes through.
				if l, ok := a.Value.Any().(slog.Level); ok {
					return slog.String("severity", severity(l))
				}
			case slog.MessageKey:
				a.Key = "message"
			}
			return a
		},
	}))
}

func severity(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERROR"
	case l >= slog.LevelWarn:
		return "WARNING"
	case l >= slog.LevelInfo:
		return "INFO"
	default:
		return "DEBUG"
	}
}

// ParseLevel maps a LOG_LEVEL value to a slog level. Empty means info.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown log level %q (want debug, info, warn or error)", s)
}

type requestIDKey struct{}

// NewRequestID returns a random ID such as "req_3f9a1c0b7d2e".
func NewRequestID() string {
	b := make([]byte, 6)
	rand.Read(b) // crypto/rand.Read never returns an error
	return "req_" + hex.EncodeToString(b)
}

// WithRequestID returns a context that carries id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID returns the ID stored by WithRequestID, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}
