package docql

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The event types a DocQL webhook delivers today (D-15). WebhookEvent.Type is
// an open string: compare it with these constants, and treat any other value
// as an event type this SDK does not know yet.
const (
	// EventJobSucceeded: a job finished and its result is ready.
	EventJobSucceeded = "job.succeeded"
	// EventJobFailed: a job ended with an error.
	EventJobFailed = "job.failed"
	// EventJobCancelled: a job was cancelled before it finished.
	EventJobCancelled = "job.cancelled"
	// EventWebhookTest: the console's "send test event" delivery; its Data is
	// empty, and JobID and Status are "".
	EventWebhookTest = "webhook.test"
)

// Webhook validation failures are sentinel errors matched with errors.Is,
// like the client's own (D-12); they are never *docql.WebhookError, which is
// only for a delivery that fails verification.
var (
	// ErrInvalidWebhookSecret is returned by VerifyWebhook when the secret
	// list is empty, or a secret is not whsec_ followed by canonical padded
	// base64 of 24 to 64 bytes. The message names the 1-based position and
	// the problem, never any character of the secret.
	ErrInvalidWebhookSecret = errors.New("docql: invalid webhook secret")
	// ErrInvalidWebhookTolerance is returned by VerifyWebhook when the
	// WithWebhookTolerance value is zero or less.
	ErrInvalidWebhookTolerance = errors.New("docql: invalid webhook tolerance")
)

const (
	defaultWebhookTolerance = 300 * time.Second
	webhookSecretPrefix     = "whsec_"
	webhookSecretMinBytes   = 24
	webhookSecretMaxBytes   = 64
	webhookSignatureBytes   = 32
	webhookTimestampDigits  = 12
	// webhookSecretTrim is exactly space, tab, LF and CR (D-29): never the
	// Unicode-aware strings.TrimSpace set.
	webhookSecretTrim = " \t\r\n"

	headerWebhookID        = "webhook-id"
	headerWebhookTimestamp = "webhook-timestamp"
	headerWebhookSignature = "webhook-signature"
)

// The seven WebhookError reasons (D-14). They are not exported in 0.4.0
// (D-28) and are not part of the Reason* API error set.
const (
	webhookMissingHeader          = "missing_header"
	webhookInvalidTimestamp       = "invalid_timestamp"
	webhookInvalidSignatureHeader = "invalid_signature_header"
	webhookSignatureMismatch      = "signature_mismatch"
	webhookTimestampTooOld        = "timestamp_too_old"
	webhookTimestampTooNew        = "timestamp_too_new"
	webhookInvalidPayload         = "invalid_payload"
)

// webhookMessages holds the one fixed sentence each reason carries.
var webhookMessages = map[string]string{
	webhookMissingHeader: "a webhook-id, webhook-timestamp or webhook-signature header is absent, " +
		"empty, or repeated with different values",
	webhookInvalidTimestamp:       "the webhook-timestamp header is not 1 to 12 ASCII digits",
	webhookInvalidSignatureHeader: "the webhook-signature header has no well-formed v1 signature",
	webhookSignatureMismatch:      "no v1 signature matches any secret (wrong secret or a changed body)",
	webhookTimestampTooOld:        "the webhook-timestamp is older than the tolerance allows",
	webhookTimestampTooNew:        "the webhook-timestamp is further in the future than the tolerance allows",
	webhookInvalidPayload:         "the signed body is not a DocQL webhook event",
}

// WebhookEvent is a verified DocQL webhook event (D-08, D-25).
type WebhookEvent struct {
	// ID is the event id. It stays the same across delivery retries, so
	// dedupe on it: VerifyWebhook does not detect replays.
	ID string
	// Type is an open string: compare it with EventJobSucceeded,
	// EventJobFailed, EventJobCancelled or EventWebhookTest.
	Type string
	// Timestamp is when the event happened, in UTC.
	Timestamp time.Time
	// JobID is Data["job_id"] when that is a string, else "".
	JobID string
	// Status is Data["status"] when that is a string, else "".
	Status JobStatus
	// Data is the event's raw data object, unknown keys included; numbers
	// are json.Number. It is never nil (a webhook.test event has an empty
	// map).
	Data map[string]any
}

// WebhookError means the delivery failed verification: do not act on it
// (D-13, D-14). Match it with errors.As. It is not *docql.Error, because no
// API call failed.
type WebhookError struct {
	// Reason is one of missing_header, invalid_timestamp,
	// invalid_signature_header, signature_mismatch, timestamp_too_old,
	// timestamp_too_new or invalid_payload.
	Reason string
}

// Error returns the reason and a fixed sentence: never a secret, the
// expected signature or a received signature.
func (e *WebhookError) Error() string {
	msg, ok := webhookMessages[e.Reason]
	if !ok {
		msg = "webhook verification failed"
	}
	return "docql: webhook " + e.Reason + ": " + msg
}

// webhookSettings carries the options of one VerifyWebhook call.
type webhookSettings struct {
	tolerance time.Duration
}

// WebhookOption configures a single VerifyWebhook call.
type WebhookOption func(*webhookSettings)

// WithWebhookTolerance sets the allowed clock skew between the
// webhook-timestamp header and now, either way and inclusive (default 300
// seconds). A d of zero or less makes VerifyWebhook return an error matched
// by ErrInvalidWebhookTolerance. When given more than once, the last wins.
func WithWebhookTolerance(d time.Duration) WebhookOption {
	return func(s *webhookSettings) { s.tolerance = d }
}

// VerifyWebhook verifies one DocQL webhook delivery and returns its event
// (D-06, D-07). A receiver needs no Client and no API key.
//
// Pass the raw request body exactly as received, before any JSON decoding,
// the request headers (names match without regard to ASCII case, so both
// r.Header and a map with raw lower-case keys work) and the endpoint's
// webhook secret, or several secrets while a rotation overlaps. Any secret
// may match any v1 signature.
//
// It returns an error matched by errors.Is to ErrInvalidWebhookTolerance or
// ErrInvalidWebhookSecret when the options or secrets are invalid, before
// the request is read, and a *WebhookError when the delivery is not an
// authentic, timely DocQL webhook event. The helper does not detect replays:
// dedupe on WebhookEvent.ID.
func VerifyWebhook(body []byte, header http.Header, secrets []string, opts ...WebhookOption) (WebhookEvent, error) {
	return verifyWebhookAt(time.Now(), body, header, secrets, opts...)
}

// verifyWebhookAt is VerifyWebhook against a given clock (D-12). It never
// reads the package-wide now used by Retry-After parsing.
func verifyWebhookAt(now time.Time, body []byte, header http.Header, secrets []string, opts ...WebhookOption) (WebhookEvent, error) {
	settings := webhookSettings{tolerance: defaultWebhookTolerance}
	for _, opt := range opts {
		opt(&settings)
	}
	if settings.tolerance <= 0 {
		return WebhookEvent{}, fmt.Errorf("docql: webhook tolerance must be greater than zero: %w", ErrInvalidWebhookTolerance)
	}
	keys, err := decodeWebhookSecrets(secrets)
	if err != nil {
		return WebhookEvent{}, err
	}

	ids := headerValues(header, headerWebhookID)
	timestamps := headerValues(header, headerWebhookTimestamp)
	signatures := headerValues(header, headerWebhookSignature)
	if len(ids) == 0 || len(timestamps) == 0 || len(signatures) == 0 {
		return WebhookEvent{}, &WebhookError{Reason: webhookMissingHeader}
	}
	msgID, ok := single(ids)
	if !ok {
		return WebhookEvent{}, &WebhookError{Reason: webhookMissingHeader}
	}
	timestamp, ok := single(timestamps)
	if !ok {
		return WebhookEvent{}, &WebhookError{Reason: webhookMissingHeader}
	}

	if len(timestamp) > webhookTimestampDigits || !isASCIIDigits(timestamp) {
		return WebhookEvent{}, &WebhookError{Reason: webhookInvalidTimestamp}
	}
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return WebhookEvent{}, &WebhookError{Reason: webhookInvalidTimestamp}
	}

	entries := signatureEntries(strings.Join(signatures, " "))
	if len(entries) == 0 {
		return WebhookEvent{}, &WebhookError{Reason: webhookInvalidSignatureHeader}
	}
	if !anySignatureMatches(keys, msgID, timestamp, body, entries) {
		return WebhookEvent{}, &WebhookError{Reason: webhookSignatureMismatch}
	}

	// Whole seconds against whole seconds: tolerance/time.Second floors, which
	// is exact for an integer skew, and nothing here can overflow (ts has at
	// most 12 digits).
	skew := now.Unix() - ts
	limit := int64(settings.tolerance / time.Second)
	if skew > limit {
		return WebhookEvent{}, &WebhookError{Reason: webhookTimestampTooOld}
	}
	if -skew > limit {
		return WebhookEvent{}, &WebhookError{Reason: webhookTimestampTooNew}
	}
	return parseWebhookEvent(body)
}

// decodeWebhookSecrets validates every secret before anything is read from
// the request (D-09, D-10).
func decodeWebhookSecrets(secrets []string) ([][]byte, error) {
	if len(secrets) == 0 {
		return nil, fmt.Errorf("docql: no webhook secrets: pass at least one webhook secret: %w", ErrInvalidWebhookSecret)
	}
	keys := make([][]byte, 0, len(secrets))
	for i, secret := range secrets {
		text := strings.Trim(secret, webhookSecretTrim)
		problem := ""
		var raw []byte
		switch {
		case !strings.HasPrefix(text, webhookSecretPrefix):
			problem = "does not start with " + webhookSecretPrefix
		default:
			var ok bool
			raw, ok = canonicalBase64(text[len(webhookSecretPrefix):])
			if !ok {
				problem = "is not canonical padded base64"
			} else if len(raw) < webhookSecretMinBytes || len(raw) > webhookSecretMaxBytes {
				problem = fmt.Sprintf("must decode to %d to %d bytes", webhookSecretMinBytes, webhookSecretMaxBytes)
			}
		}
		if problem != "" {
			return nil, fmt.Errorf("docql: webhook secret %d: %s: %w", i+1, problem, ErrInvalidWebhookSecret)
		}
		keys = append(keys, raw)
	}
	return keys, nil
}

// canonicalBase64 returns the decoded bytes when s is canonical padded
// standard base64. StdEncoding skips CR and LF and accepts non-zero trailing
// bits, so the re-encoding must give s back.
func canonicalBase64(s string) ([]byte, bool) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil || base64.StdEncoding.EncodeToString(raw) != s {
		return nil, false
	}
	return raw, true
}

// headerValues collects every non-empty value of every key that is all ASCII
// and equals name after an ASCII lower-case fold (D-29). It reads the raw map
// keys, never the canonicalizing Header.Get, so raw lower-case keys work, and
// it never folds Unicode, so a look-alike such as U+212A does not match. A
// nil header has no values.
func headerValues(h http.Header, name string) []string {
	var out []string
	for key, values := range h {
		if !asciiFoldEqual(key, name) {
			continue
		}
		for _, v := range values {
			if v != "" {
				out = append(out, v)
			}
		}
	}
	return out
}

// asciiFoldEqual reports whether key is all ASCII and equals lower (already
// lower case) after folding A-Z to a-z.
func asciiFoldEqual(key, lower string) bool {
	if len(key) != len(lower) {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if c >= utf8.RuneSelf {
			return false
		}
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lower[i] {
			return false
		}
	}
	return true
}

// single returns the one distinct value of vs; identical repeats collapse.
func single(vs []string) (string, bool) {
	for _, v := range vs[1:] {
		if v != vs[0] {
			return "", false
		}
	}
	return vs[0], true
}

// signatureEntries splits on U+0020 only, drops empty pieces and keeps each
// "v1," entry whose rest is canonical padded base64 of exactly 32 bytes; any
// other piece is skipped.
func signatureEntries(header string) [][]byte {
	var out [][]byte
	for _, piece := range strings.Split(header, " ") {
		rest, ok := strings.CutPrefix(piece, "v1,")
		if !ok {
			continue
		}
		if raw, ok := canonicalBase64(rest); ok && len(raw) == webhookSignatureBytes {
			out = append(out, raw)
		}
	}
	return out
}

// anySignatureMatches computes HMAC-SHA256 over id.timestamp.body once per
// secret, with the timestamp text exactly as received, and compares it in
// constant time with every usable entry.
func anySignatureMatches(keys [][]byte, msgID, timestamp string, body []byte, entries [][]byte) bool {
	for _, key := range keys {
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(msgID))
		mac.Write([]byte{'.'})
		mac.Write([]byte(timestamp))
		mac.Write([]byte{'.'})
		mac.Write(body)
		want := mac.Sum(nil)
		for _, entry := range entries {
			if hmac.Equal(want, entry) {
				return true
			}
		}
	}
	return false
}

// parseWebhookEvent decodes the signed body into exact-key maps: Go's
// case-insensitive struct matching would take "ID" for "id" (D-27).
func parseWebhookEvent(body []byte) (WebhookEvent, error) {
	invalid := &WebhookError{Reason: webhookInvalidPayload}
	// encoding/json replaces invalid UTF-8 silently, so check it first.
	if !utf8.Valid(body) {
		return WebhookEvent{}, invalid
	}
	// Exactly one JSON object, then EOF: dec.More() is false before a stray
	// } or ], so the second Decode must report io.EOF.
	var top map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&top); err != nil || top == nil {
		return WebhookEvent{}, invalid
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return WebhookEvent{}, invalid
	}

	id, ok := jsonString(top["id"])
	if !ok || id == "" {
		return WebhookEvent{}, invalid
	}
	typ, ok := jsonString(top["type"])
	if !ok || typ == "" {
		return WebhookEvent{}, invalid
	}
	stamp, ok := jsonString(top["timestamp"])
	if !ok {
		return WebhookEvent{}, invalid
	}
	when, ok := parseWebhookTimestamp(stamp)
	if !ok {
		return WebhookEvent{}, invalid
	}
	raw, ok := top["data"]
	if !ok {
		return WebhookEvent{}, invalid
	}
	var data map[string]any
	dd := json.NewDecoder(bytes.NewReader(raw))
	dd.UseNumber()
	if err := dd.Decode(&data); err != nil || data == nil {
		return WebhookEvent{}, invalid
	}

	event := WebhookEvent{ID: id, Type: typ, Timestamp: when, Data: data}
	if v, ok := data["job_id"].(string); ok {
		event.JobID = v
	}
	if v, ok := data["status"].(string); ok {
		event.Status = JobStatus(v)
	}
	return event, nil
}

// jsonString decodes raw as a JSON string; an absent key, null or any other
// JSON type is not one.
func jsonString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// parseWebhookTimestamp accepts only YYYY-MM-DDTHH:MM:SS[.1-9 digits]Z in
// ASCII and returns it in UTC. time.Parse alone would also take offsets and
// other forms the console never sends; after the shape check it range-checks
// the month, the day (February 30), hour 0-23, minute 0-59 and second 0-59
// (no leap second). Year 0000 is refused too: time.Parse accepts it, but the
// other SDKs cannot represent it, so all three treat it as invalid_payload.
func parseWebhookTimestamp(s string) (time.Time, bool) {
	if !webhookTimestampShape(s) {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.Year() == 0 {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// webhookTimestampShape matches ^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,9})?Z$
// with ASCII digits only.
func webhookTimestampShape(s string) bool {
	const head = "dddd-dd-ddTdd:dd:dd"
	if len(s) < len(head)+1 || s[len(s)-1] != 'Z' {
		return false
	}
	for i := 0; i < len(head); i++ {
		if head[i] == 'd' {
			if s[i] < '0' || s[i] > '9' {
				return false
			}
		} else if s[i] != head[i] {
			return false
		}
	}
	rest := s[len(head) : len(s)-1]
	if rest == "" {
		return true
	}
	return len(rest) >= 2 && len(rest) <= 10 && rest[0] == '.' && isASCIIDigits(rest[1:])
}
