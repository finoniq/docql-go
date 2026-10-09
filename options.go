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
	mode     *Mode
	filename *string
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
