package partner

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"
)

// Sentinels for errors.Is. An *APIError matches them by status code, so
// callers never inspect status codes themselves.
var (
	ErrRateLimited = errors.New("rate limited")
	ErrUnavailable = errors.New("unavailable")
	ErrNotFound    = errors.New("not found")
)

// APIError is a failed partner call after all the attempts we were willing
// to make. Its message is written for the agent that will read it.
type APIError struct {
	Partner    string
	StatusCode int           // 0 when no response arrived (network error, timeout)
	Attempts   int           // HTTP attempts made
	RetryAfter time.Duration // what the partner asked for on 429/503, if anything
	Body       string        // start of the response body on other 4xx
	Err        error         // transport or context error, if any
}

func (e *APIError) Error() string {
	p := "partner " + e.Partner + ": "
	switch {
	case e.StatusCode == http.StatusTooManyRequests:
		if e.RetryAfter > 0 {
			return p + "rate limited (429), retry after " + e.RetryAfter.String()
		}
		return p + "rate limited (429)"
	case e.StatusCode >= 500:
		return fmt.Sprintf("%sunavailable (%d) after %s", p, e.StatusCode, attempts(e.Attempts))
	case e.StatusCode == http.StatusNotFound:
		return p + "not found (404)"
	case e.StatusCode >= 400:
		return fmt.Sprintf("%srejected (%d): %s", p, e.StatusCode, e.Body)
	case e.StatusCode != 0:
		return fmt.Sprintf("%sunexpected response (%d)", p, e.StatusCode)
	case errors.Is(e.Err, context.Canceled):
		return p + "request cancelled"
	case isTimeout(e.Err):
		return fmt.Sprintf("%stimed out after %s", p, attempts(e.Attempts))
	default:
		return fmt.Sprintf("%sunreachable after %s: %v", p, attempts(e.Attempts), e.Err)
	}
}

func (e *APIError) Unwrap() error { return e.Err }

// Rejected reports a definite "no" from the partner: a 4xx other than 429.
// Nothing was done, and repeating the same request won't change that.
func (e *APIError) Rejected() bool {
	return e.StatusCode >= 400 && e.StatusCode < 500 && e.StatusCode != http.StatusTooManyRequests
}

// Is maps status codes to the sentinels. A timeout is not "unavailable": the
// partner may have done the work and just been slow to say so.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrRateLimited:
		return e.StatusCode == http.StatusTooManyRequests
	case ErrUnavailable:
		return e.StatusCode >= 500 ||
			(e.StatusCode == 0 && e.Err != nil && !isTimeout(e.Err) && !errors.Is(e.Err, context.Canceled))
	case ErrNotFound:
		return e.StatusCode == http.StatusNotFound
	}
	return false
}

func attempts(n int) string {
	if n == 1 {
		return "1 attempt"
	}
	return strconv.Itoa(n) + " attempts"
}

// isTimeout covers both our context deadline and http.Client.Timeout.
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// parseRetryAfter reads either delay-seconds or an HTTP date.
func parseRetryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil && s >= 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now)
	}
	return 0
}
