package docql

import (
	"net/http"
	"time"
)

// config carries everything NewClient resolves (D-01). Options only record
// values and whether each was explicitly given; validation happens in
// NewClient and QueryDocument.
type config struct {
	apiKey        string
	apiKeySet     bool
	apiURL        string
	apiURLSet     bool
	timeout       time.Duration
	timeoutSet    bool
	httpClient    *http.Client
	httpClientSet bool
}

// Option configures NewClient (D-01).
type Option func(*config)

// WithAPIKey sets the API key explicitly (D-01, D-15). An explicitly given
// empty or whitespace-only key is an error — there is no fallback to
// DOCQL_API_KEY for it.
func WithAPIKey(key string) Option {
	return func(c *config) { c.apiKey = key; c.apiKeySet = true }
}

// WithAPIURL sets the API base URL explicitly (D-01). The name mirrors the
// DOCQL_API_URL environment variable, not an ecosystem-typical WithBaseURL.
func WithAPIURL(u string) Option {
	return func(c *config) { c.apiURL = u; c.apiURLSet = true }
}

// WithTimeout sets the overall per-call deadline (D-02). The SDK applies it
// with context.WithTimeout on every request; a caller deadline that is
// earlier wins. The deadline covers upload, server wait and response read.
func WithTimeout(d time.Duration) Option {
	return func(c *config) { c.timeout = d; c.timeoutSet = true }
}

// WithHTTPClient injects a caller-owned *http.Client (D-02, D-15). The SDK
// uses a shallow copy with the client's Timeout cleared and redirects
// refused; the caller's struct is never modified.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *config) { c.httpClient = hc; c.httpClientSet = true }
}

// querySettings carries the per-call options of QueryDocument (D-04). Nil
// means "not given".
type querySettings struct {
	mode           *Mode
	filename       *string
	idempotencyKey *string
}

// QueryOption configures a single QueryDocument call (D-04).
type QueryOption func(*querySettings)

// WithMode sets the extraction mode for this call (D-08). When it is not
// given, mode is omitted from the request.
func WithMode(m Mode) QueryOption {
	return func(q *querySettings) { q.mode = &m }
}

// WithFilename names the file part for this call (D-06). It always wins,
// verbatim, over any name carried by the File.
func WithFilename(name string) QueryOption {
	return func(q *querySettings) { q.filename = &name }
}

// WithIdempotencyKey sets the Idempotency-Key of one SubmitDocument call
// (D-07). Without it SubmitDocument generates a fresh random key. The key
// must be 1-255 printable ASCII characters (0x20-0x7E), else SubmitDocument
// returns ErrInvalidIdempotencyKey before sending anything. QueryDocument
// does not take it and returns ErrIdempotencyKeyOnQuery.
func WithIdempotencyKey(key string) QueryOption {
	return func(q *querySettings) { q.idempotencyKey = &key }
}

// waitSettings carries the per-call options of Wait. Zero means "not given".
type waitSettings struct {
	pollInterval time.Duration
}

// WaitOption configures a single Wait call.
type WaitOption func(*waitSettings)

// WithPollInterval sets the shortest sleep between two polls of one Wait
// call: each sleep is the larger of d and the server's Retry-After. A d of
// zero or less is ignored.
func WithPollInterval(d time.Duration) WaitOption {
	return func(w *waitSettings) {
		if d > 0 {
			w.pollInterval = d
		}
	}
}
