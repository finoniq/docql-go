package docql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Client-side validation failures are sentinel errors matched with errors.Is;
// they are never *docql.Error or *docql.ConnectionError (D-12).
var (
	// ErrMissingAPIKey is returned when no API key can be resolved, or an
	// explicitly given key is empty or whitespace-only.
	ErrMissingAPIKey = errors.New("docql: API key missing: pass WithAPIKey or set DOCQL_API_KEY")
	// ErrInvalidAPIKey is returned when the API key, after surrounding
	// whitespace is trimmed, still contains a control character (CR, LF, NUL
	// or any other). The key is never part of the error text.
	ErrInvalidAPIKey = errors.New("docql: API key contains a control character: check WithAPIKey or DOCQL_API_KEY")
	// ErrInvalidAPIURL is returned when the API URL is empty or not an
	// http:// or https:// URL.
	ErrInvalidAPIURL = errors.New("docql: api_url must be an http:// or https:// URL: pass WithAPIURL or set DOCQL_API_URL")
	// ErrInvalidTimeout is returned for a WithTimeout value of zero or less.
	ErrInvalidTimeout = errors.New("docql: timeout must be greater than zero: pass a positive WithTimeout value")
	// ErrNilClient is returned when QueryDocument is called on a nil *Client
	// or a zero docql.Client{}; build clients with NewClient.
	ErrNilClient = errors.New("docql: client is nil or not built by NewClient")
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

	// ErrInvalidIdempotencyKey is returned by SubmitDocument when the
	// WithIdempotencyKey value is not 1-255 printable ASCII characters. The
	// key is never part of the error text.
	ErrInvalidIdempotencyKey = errors.New("docql: idempotency key must be 1-255 printable ASCII characters (0x20-0x7E)")
	// ErrNoJobID is returned by GetJob, Wait and CancelJob for an empty job
	// id, before any request is sent.
	ErrNoJobID = errors.New("docql: no job id")
	// ErrIdempotencyKeyOnQuery is returned by QueryDocument when it is given
	// WithIdempotencyKey: the key belongs to SubmitDocument.
	ErrIdempotencyKeyOnQuery = errors.New("docql: WithIdempotencyKey applies to SubmitDocument, not QueryDocument")

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
	// or 405, an HTML 524, an empty 5xx). It never contains the API key or a
	// control character.
	Reason string
	// RequestID comes from the envelope metadata, else the X-Request-Id
	// response header; "" when neither is present. It never contains the API
	// key or a control character.
	RequestID string
	// RetryAfter is the parsed Retry-After header in whole seconds; 0 when
	// absent (D-09).
	RetryAfter time.Duration
	// Retryable is true only for 429 and 503.
	Retryable bool
	// Message is error_info from the envelope; "" when absent. It never
	// contains the API key or a control character, and is cut to 2048
	// characters.
	Message string
	// Body is the response body, made valid UTF-8, scrubbed of the API key
	// and truncated to 2048 characters.
	Body string
	// JobID is the job the error belongs to: set on every error of GetJob,
	// Wait and CancelJob, and on a failed job's error (D-08); "" otherwise.
	JobID string
	// IdempotencyKey is the Idempotency-Key the failed SubmitDocument call
	// sent, so the caller can retry safely with the same key (D-08); "" for
	// every other call. It is never part of Error().
	IdempotencyKey string
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
		if e.JobID != "" {
			extras = append(extras, "job_id="+printableJobID(e.JobID))
		}
		if len(extras) > 0 {
			text += " (" + strings.Join(extras, ", ") + ")"
		}
		return text
	}
	text := fmt.Sprintf("%d (non-DocQL response): %s", e.Status, snippet(e.Body))
	var tail []string
	if e.RequestID != "" {
		tail = append(tail, "request_id="+e.RequestID)
	}
	if e.JobID != "" {
		tail = append(tail, "job_id="+printableJobID(e.JobID))
	}
	if len(tail) > 0 {
		text += " (" + strings.Join(tail, ", ") + ")"
	}
	return text
}

// printableJobID renders a job id inside an error string: the id is opaque
// and may come from the caller or the server, so control characters become
// spaces and a hostile id cannot forge log lines.
func printableJobID(id string) string {
	return sanitizeServerString(id, "", 0)
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
	// IdempotencyKey is the Idempotency-Key the SubmitDocument request
	// carried when the connection failed, so the caller can retry safely
	// with the same key (D-08); "" for every other call. It is never part of
	// Error().
	IdempotencyKey string

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

// sanitizeServerString makes a server-controlled string safe to store on an
// error or result and to print in Error() (12-REVIEW WR-01): invalid UTF-8
// becomes U+FFFD, every control character (CR and LF included) becomes a
// space so a hostile server cannot forge log lines, every occurrence of the
// key is then replaced with ***, and the text is cut to maxRunes runes when
// maxRunes > 0. The scrub runs after the control-character pass, so a key
// split by a control character cannot reassemble, and before the cut, so a
// key straddling the cut cannot survive as a fragment.
func sanitizeServerString(s, key string, maxRunes int) string {
	text := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, string(utf8.RuneError)))
	text = scrub(text, key)
	if maxRunes > 0 {
		if runes := []rune(text); len(runes) > maxRunes {
			text = string(runes[:maxRunes])
		}
	}
	return text
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
// with a string reason. No stored string (Body, Message, Reason, RequestID)
// ever contains the API key, even when the server echoes it back, and Reason,
// RequestID and Message carry no control characters (12-REVIEW WR-01).
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
				e.Reason = sanitizeServerString(*reason, key, 0)
				var info *string
				if json.Unmarshal(doc["error_info"], &info) == nil && info != nil {
					e.Message = sanitizeServerString(*info, key, errorBodyKeepChars)
				}
				var rid *string
				if json.Unmarshal(md["request_id"], &rid) == nil && rid != nil {
					e.RequestID = sanitizeServerString(*rid, key, 0)
				}
			}
		}
	}
	if e.RequestID == "" {
		e.RequestID = sanitizeServerString(h.Get("X-Request-Id"), key, 0)
	}
	e.RetryAfter = parseRetryAfter(h.Get("Retry-After"), now())
	e.Body = scrubAndTruncate(body, key)
	return e
}

// WaitError is returned by Wait when the wait stopped before the job reached
// a terminal status: the caller's context was cancelled, or the wait deadline
// (the context's, else 15 minutes) passed (D-06). The job itself was not
// cancelled and may still be running; poll it with GetJob, wait again, or
// cancel it with CancelJob. Unwrap returns the context error, so
// errors.Is(err, ctx.Err()), errors.Is(err, context.DeadlineExceeded) and
// errors.Is(err, context.Canceled) work.
type WaitError struct {
	// JobID is the job the wait was for.
	JobID string

	err error
}

// Error names the job and the context error, and says the job was not
// cancelled.
func (e *WaitError) Error() string {
	return fmt.Sprintf("docql: wait for job %s stopped: %v; the job was not cancelled", printableJobID(e.JobID), e.err)
}

// Unwrap returns the context error that stopped the wait.
func (e *WaitError) Unwrap() error { return e.err }

// Timeout reports whether the wait stopped because its deadline passed,
// rather than because the caller cancelled it.
func (e *WaitError) Timeout() bool { return errors.Is(e.err, context.DeadlineExceeded) }
