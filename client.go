package docql

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Client is a client for the DocQL document-extraction API (D-01). A *Client
// is safe for concurrent use by multiple goroutines and has no Close method
// (D-03).
type Client struct {
	cfg *config
}

// sdkDialer is the dialer of the SDK-built transport: the connect limit on
// dial, with keep-alives (D-02). It is a package-level var so tests can read
// the connect limit.
var sdkDialer = &net.Dialer{
	Timeout:   connectTimeout,
	KeepAlive: 30 * time.Second,
}

// sdkTransport builds, once, the transport for clients without an injected
// *http.Client: a clone of http.DefaultTransport with the connect limit
// applied to dial and TLS handshake (D-02). It is shared and never torn down
// by the SDK (D-03).
var sdkTransport = sync.OnceValue(func() http.RoundTripper {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = sdkDialer.DialContext
	tr.TLSHandshakeTimeout = connectTimeout
	return tr
})

// refuseRedirect refuses every redirect (D-15): the 3xx response is decoded
// like any other non-2xx, so X-API-Key never leaves the configured host.
func refuseRedirect(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

// NewClient builds a client (D-01). Every value is an option; each falls back
// to its environment variable (a whitespace-only value counts as unset), and
// the URL additionally to the production default. It returns an error, and no
// client, when no usable key can be resolved, the URL is not an http:// or
// https:// URL, or the timeout is not positive.
func NewClient(opts ...Option) (*Client, error) {
	cfg := &config{timeout: defaultTimeout}
	for _, opt := range opts {
		opt(cfg)
	}

	var key string
	if cfg.apiKeySet {
		if strings.TrimSpace(cfg.apiKey) == "" {
			return nil, fmt.Errorf("%w: the explicitly given WithAPIKey value is empty", ErrMissingAPIKey)
		}
		key = cfg.apiKey
	} else {
		key = os.Getenv(envAPIKey)
		if strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("%w: WithAPIKey was not given and %s is unset", ErrMissingAPIKey, envAPIKey)
		}
	}

	var urlStr string
	if cfg.apiURLSet {
		urlStr = cfg.apiURL
	} else {
		urlStr = strings.TrimSpace(os.Getenv(envAPIURL))
		if urlStr == "" {
			urlStr = defaultAPIURL
		}
	}
	lowered := strings.ToLower(urlStr)
	if !strings.HasPrefix(lowered, "http://") && !strings.HasPrefix(lowered, "https://") {
		return nil, fmt.Errorf("%w (got %q)", ErrInvalidAPIURL, urlStr)
	}
	parsed, perr := url.Parse(urlStr)
	if perr != nil {
		return nil, fmt.Errorf("%w (not a URL Go can parse: %v)", ErrInvalidAPIURL, perr)
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("%w (no host in %q)", ErrInvalidAPIURL, urlStr)
	}
	apiURL := strings.TrimRight(urlStr, "/")

	timeout := defaultTimeout
	if cfg.timeoutSet {
		if cfg.timeout <= 0 {
			return nil, fmt.Errorf("%w (got %s)", ErrInvalidTimeout, cfg.timeout)
		}
		timeout = cfg.timeout
	}

	var httpClient *http.Client
	if cfg.httpClientSet && cfg.httpClient != nil {
		copied := *cfg.httpClient
		copied.Timeout = 0 // a shorter caller default must not shorten the call (D-02)
		copied.CheckRedirect = refuseRedirect
		httpClient = &copied
	} else {
		httpClient = &http.Client{
			Transport:     sdkTransport(),
			CheckRedirect: refuseRedirect,
		}
	}

	cfg.apiKey = key
	cfg.apiURL = apiURL
	cfg.timeout = timeout
	cfg.httpClient = httpClient
	return &Client{cfg: cfg}, nil
}

// String shows only the API URL; the API key never prints (D-13). The same
// text serves GoString for the %#v verb. Both use value receivers on purpose:
// fmt cannot call methods through an unexported field, so a pointer-only
// receiver would let %s on a dereferenced Client fall into the bad-verb
// fallback that deep-prints the unexported config — and the key. With value
// receivers every verb on both the pointer and the dereferenced value renders
// this masked form.
func (c Client) String() string {
	if c.cfg == nil {
		return `docql.Client{APIURL: ""}`
	}
	return fmt.Sprintf("docql.Client{APIURL: %q}", c.cfg.apiURL)
}

// GoString mirrors String so %#v masks the key too (D-13).
func (c Client) GoString() string { return c.String() }

// userAgent is sent on every request (D-14).
func userAgent() string {
	return fmt.Sprintf("docql-go/%s (%s; %s/%s)", Version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// connectionError maps a transport failure to *ConnectionError (D-10). When
// the deadline or cancellation fired and the cause does not already wrap it,
// the context error is joined into the cause with two %w verbs, so
// errors.Is(err, context.DeadlineExceeded) and errors.Is(err,
// context.Canceled) keep working. Messages carry the size hint once the known
// upload passes the API ceiling, because an early 413 can be lost as a reset,
// and are scrubbed of the key before they leave the SDK.
func (c *Client) connectionError(ctx context.Context, cause error, up *upload) *ConnectionError {
	if errors.Is(cause, errShortRead) {
		message := fmt.Sprintf("docql: the file changed size during upload: %v", cause)
		return &ConnectionError{msg: scrub(message, c.cfg.apiKey), cause: cause}
	}
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(cause, ctxErr) {
		cause = fmt.Errorf("%w: %w", cause, ctxErr)
	}
	timeout := ctx.Err() == context.DeadlineExceeded
	var ne net.Error
	if errors.As(cause, &ne) && ne.Timeout() {
		timeout = true
	}
	message := fmt.Sprintf("docql: connection to %s failed: %v", c.cfg.apiURL, cause)
	if timeout {
		message = fmt.Sprintf("docql: request timed out after %s: %v", c.cfg.timeout, cause)
	}
	if up != nil && up.fileSize > maxUploadBytes {
		message += fmt.Sprintf(" (the upload is %d bytes; the API rejects files over 25 MiB)", up.fileSize)
	}
	return &ConnectionError{msg: scrub(message, c.cfg.apiKey), cause: cause, timeout: timeout}
}

// QueryDocument uploads one document with an instruction and returns its
// extraction result (D-04). The file and the instruction are required;
// WithMode and WithFilename tune the single call. Nothing here retries: calls
// are paid. Every client-side check happens before a byte is sent.
func (c *Client) QueryDocument(ctx context.Context, file File, instr Instruction, opts ...QueryOption) (*QueryResult, error) {
	if ctx == nil {
		return nil, ErrNilContext
	}
	if instr.kind == instructionNone {
		return nil, ErrNoInstruction
	}
	if file.kind == fileNone {
		return nil, ErrNoFile
	}
	var qs querySettings
	for _, opt := range opts {
		opt(&qs)
	}
	up, err := prepareUpload(file, instr, qs)
	if err != nil {
		return nil, err
	}
	// A file the SDK opened from a path is closed by the SDK on every path,
	// after the response is read (D-07). The body's own Close is idempotent
	// with this.
	defer up.closer.close()

	ctx2, cancel := context.WithTimeout(ctx, c.cfg.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx2, http.MethodPost, c.cfg.apiURL+queryPath, up.body)
	if err != nil {
		return nil, c.connectionError(ctx2, err, up)
	}
	// Exactly three per-request headers (D-14, D-15): the key travels on the
	// request only, never as a client default, and no X-Request-Id or
	// x-docql-* header is ever set.
	req.Header.Set("X-API-Key", c.cfg.apiKey)
	req.Header.Set("Content-Type", up.contentType)
	req.Header.Set("User-Agent", userAgent())
	req.ContentLength = up.contentLength
	req.GetBody = up.getBody

	resp, err := c.cfg.httpClient.Do(req)
	if err != nil {
		return nil, c.connectionError(ctx2, err, up)
	}
	defer resp.Body.Close()

	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	var body []byte
	if ok {
		// The success body is read fully, inside the deadline (D-02).
		body, err = io.ReadAll(resp.Body)
	} else {
		body, err = io.ReadAll(io.LimitReader(resp.Body, errorBodyReadLimit))
	}
	if err != nil {
		return nil, c.connectionError(ctx2, err, up)
	}

	if ok {
		res, perr := parseResult(resp.StatusCode, resp.Header, body, c.cfg.apiKey)
		if perr != nil {
			return nil, perr
		}
		return res, nil
	}
	return nil, decodeError(resp.StatusCode, resp.Header, body, c.cfg.apiKey)
}
