package docql

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Client-side validation failures are sentinel errors matched with errors.Is;
// they are never *docql.Error or *docql.ConnectionError (D-12).
var (
	// ErrMissingAPIKey is returned when no API key can be resolved, or an
	// explicitly given key is empty or whitespace-only.
	ErrMissingAPIKey = errors.New("docql: API key missing: pass WithAPIKey or set DOCQL_API_KEY")
	// ErrInvalidAPIURL is returned when the API URL is empty or not an
	// http:// or https:// URL.
	ErrInvalidAPIURL = errors.New("docql: api_url must be an http:// or https:// URL: pass WithAPIURL or set DOCQL_API_URL")
	// ErrInvalidTimeout is returned for a WithTimeout value of zero or less.
	ErrInvalidTimeout = errors.New("docql: timeout must be greater than zero: pass a positive WithTimeout value")
	// ErrNilContext is returned when QueryDocument receives a nil context.
	ErrNilContext = errors.New("docql: context must not be nil")
	// ErrNoInstruction is returned when the Instruction is the zero value.
	ErrNoInstruction = errors.New("docql: no instruction: pass Query(...) or Prompt(...)")
	// ErrNoFile is returned when the File is the zero value or a kind this
	// call cannot send.
	ErrNoFile = errors.New("docql: no file: pass FileBytes, FilePath or FileReader")
	// ErrMissingFilename is returned when the final file-part name would be
	// empty: there is no default filename (D-06).
	ErrMissingFilename = errors.New("docql: missing filename: this file input needs WithFilename to name the file part")

	// errShortRead marks a measured file that delivered fewer bytes than its
	// measured length. Callers never match it directly: QueryDocument maps
	// any chain carrying it to a *ConnectionError naming the size change
	// (D-07).
	errShortRead = errors.New("short read")
)

// Error is the type for every non-2xx API response, matched with errors.As
// (D-09). A 2xx response the SDK cannot use is reported the same way, with an
// empty Reason and Retryable false.
type Error struct {
	// Status is the HTTP status code.
	Status int
	// Reason is the envelope reason; "" for non-DocQL bodies (a Fastify 404
	// or 405, an HTML 524, an empty 5xx).
	Reason string
	// RequestID comes from the envelope metadata, else the X-Request-Id
	// response header; "" when neither is present.
	RequestID string
	// RetryAfter is the parsed Retry-After header in whole seconds; 0 when
	// absent (D-09).
	RetryAfter time.Duration
	// Retryable is true only for 429 and 503.
	Retryable bool
	// Message is error_info from the envelope; "" when absent. It never
	// contains the API key.
	Message string
	// Body is the response body, made valid UTF-8, scrubbed of the API key
	// and truncated to 2048 characters.
	Body string
}

// Error renders one of two string forms (D-09): the envelope form
// "<status> <reason>[: <message>] (request_id=…, retry_after=Ns)" and, for
// non-DocQL bodies, "<status> (non-DocQL response): <snippet>". Neither form
// ever contains the API key.
func (e *Error) Error() string {
	if e.Reason != "" {
		text := fmt.Sprintf("%d %s", e.Status, e.Reason)
		if e.Message != "" {
			text += ": " + e.Message
		}
		var extras []string
		if e.RequestID != "" {
			extras = append(extras, "request_id="+e.RequestID)
		}
		if e.RetryAfter > 0 {
			extras = append(extras, fmt.Sprintf("retry_after=%ds", int64(e.RetryAfter/time.Second)))
		}
		if len(extras) > 0 {
			text += " (" + strings.Join(extras, ", ") + ")"
		}
		return text
	}
	text := fmt.Sprintf("%d (non-DocQL response): %s", e.Status, snippet(e.Body))
	if e.RequestID != "" {
		text += fmt.Sprintf(" (request_id=%s)", e.RequestID)
	}
	return text
}

// snippet collapses whitespace runs, trims, and cuts the body snippet to
// snippetMaxChars runes (D-09).
func snippet(body string) string {
	collapsed := strings.Join(strings.Fields(body), " ")
	if collapsed == "" {
		return "(empty body)"
	}
	runes := []rune(collapsed)
	if len(runes) > snippetMaxChars {
		runes = runes[:snippetMaxChars]
	}
	return string(runes)
}

// ConnectionError is the single transport-error type (D-10): connect, TLS,
// reset, an early close, a short read or a timeout — any failure before a
// usable HTTP response arrived. Match it with errors.As.
type ConnectionError struct {
	msg     string
	cause   error
	timeout bool
}

// Error returns the stored message, already scrubbed of the API key (D-10).
func (e *ConnectionError) Error() string { return e.msg }

// Unwrap returns the underlying transport or context error, so
// errors.Is(err, context.DeadlineExceeded) and friends keep working (D-10).
func (e *ConnectionError) Unwrap() error { return e.cause }

// Timeout reports whether the call timed out: either the SDK deadline or the
// caller's context deadline expired, or the transport surfaced a net.Error
// timeout (D-10).
func (e *ConnectionError) Timeout() bool { return e.timeout }

// scrub replaces every occurrence of key with ***; an empty key is a no-op
// (D-13).
func scrub(s, key string) string {
	if key == "" {
		return s
	}
	return strings.ReplaceAll(s, key, "***")
}

// scrubAndTruncate makes a response body safe to store on an error: invalid
// UTF-8 becomes U+FFFD, every occurrence of the key is replaced before any
// truncation (so a key straddling the cut can never survive as a fragment),
// and the text is cut to errorBodyKeepChars runes.
func scrubAndTruncate(b []byte, key string) string {
	text := scrub(strings.ToValidUTF8(string(b), string(utf8.RuneError)), key)
	runes := []rune(text)
	if len(runes) > errorBodyKeepChars {
		runes = runes[:errorBodyKeepChars]
	}
	return string(runes)
}

// now is the injectable clock for Retry-After parsing; tests stub it.
var now = time.Now

// parseRetryAfter returns the Retry-After header value as whole seconds, or 0
// when absent or unparsable (D-09). Delta-seconds pass through as-is; an
// HTTP-date becomes the seconds from now rounded up and clamped at 0, so a
// past date means "retry now" rather than a negative sleep. A value that
// cannot be a Duration is 0, never an overflow.
func parseRetryAfter(v string, now time.Time) time.Duration {
	text := strings.TrimSpace(v)
	if isASCIIDigits(text) {
		secs, err := strconv.ParseInt(text, 10, 64)
		if err != nil || secs > math.MaxInt64/int64(time.Second) {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	date, err := http.ParseTime(text)
	if err != nil {
		return 0
	}
	d := date.Sub(now)
	if d <= 0 {
		return 0
	}
	return time.Duration((d+time.Second-1)/time.Second) * time.Second
}

// isASCIIDigits reports whether s is a non-empty run of ASCII 0-9.
func isASCIIDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// decodeError decodes a non-2xx response into *Error (D-09). An envelope is
// present only when the body is a JSON object whose metadata is an object
// with a string reason. Neither the stored Body nor Message ever contains the
// API key, even when the server echoes it back.
func decodeError(status int, h http.Header, body []byte, key string) *Error {
	e := &Error{
		Status:    status,
		Retryable: status == 429 || status == 503,
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) == nil && doc != nil {
		var md map[string]json.RawMessage
		if json.Unmarshal(doc["metadata"], &md) == nil && md != nil {
			var reason *string
			if json.Unmarshal(md["reason"], &reason) == nil && reason != nil {
				e.Reason = *reason
				var info *string
				if json.Unmarshal(doc["error_info"], &info) == nil && info != nil {
					e.Message = scrub(*info, key)
				}
				var rid *string
				if json.Unmarshal(md["request_id"], &rid) == nil && rid != nil {
					e.RequestID = *rid
				}
			}
		}
	}
	if e.RequestID == "" {
		e.RequestID = h.Get("X-Request-Id")
	}
	e.RetryAfter = parseRetryAfter(h.Get("Retry-After"), now())
	e.Body = scrubAndTruncate(body, key)
	return e
}
