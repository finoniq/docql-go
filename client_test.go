package docql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordedRequest carries what one handler invocation saw on the wire.
type recordedRequest struct {
	header           http.Header
	body             []byte
	contentLength    int64
	transferEncoding []string
}

// serveCapture starts an httptest server that records every request and
// answers a minimal success envelope. Recorded requests arrive on the
// returned channel in request order.
func serveCapture(t *testing.T) (*httptest.Server, <-chan *recordedRequest) {
	t.Helper()
	got := make(chan *recordedRequest, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, rerr := io.ReadAll(r.Body)
		if rerr != nil {
			// An aborted upload never reached the wire as a request; it must
			// not be recorded as one.
			http.Error(w, "aborted body", http.StatusBadRequest)
			return
		}
		got <- &recordedRequest{
			header:           r.Header.Clone(),
			body:             body,
			contentLength:    r.ContentLength,
			transferEncoding: r.TransferEncoding,
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{},"metadata":{"request_id":"client-req-1"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

// sendProbe sends one QueryDocument(FileBytes, Query("q")) call with the given
// call options and returns the next recorded request.
func sendProbe(t *testing.T, client *Client, got <-chan *recordedRequest, callOpts ...QueryOption) *recordedRequest {
	t.Helper()
	if _, err := client.QueryDocument(context.Background(),
		FileBytes([]byte("%PDF-1.4 unit test"), "f.pdf"), Query("q"), callOpts...); err != nil {
		t.Fatalf("QueryDocument: %v", err)
	}
	return <-got
}

// captureCall runs one probe against a fresh capture server through a client
// built from opts (the server URL is appended as WithAPIURL).
func captureCall(t *testing.T, opts []Option, callOpts ...QueryOption) *recordedRequest {
	t.Helper()
	srv, got := serveCapture(t)
	client, err := NewClient(append(opts, WithAPIURL(srv.URL))...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return sendProbe(t, client, got, callOpts...)
}

// bodyPart decodes the JSON body part of a captured multipart request.
func bodyPart(t *testing.T, rec *recordedRequest) map[string]any {
	t.Helper()
	boundary, found := strings.CutPrefix(rec.header.Get("Content-Type"), "multipart/form-data; boundary=")
	if !found {
		t.Fatalf("Content-Type %q is not a multipart form", rec.header.Get("Content-Type"))
	}
	marker := []byte("Content-Disposition: form-data; name=\"body\"\r\n\r\n")
	start := bytes.Index(rec.body, marker)
	if start < 0 {
		t.Fatalf("no body part in %d-byte request", len(rec.body))
	}
	start += len(marker)
	endMark := []byte("\r\n--" + boundary + "--")
	end := bytes.Index(rec.body[start:], endMark)
	if end < 0 {
		t.Fatalf("no closing delimiter for boundary %s", boundary)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.body[start:start+end], &doc); err != nil {
		t.Fatalf("decode body part: %v", err)
	}
	return doc
}

func TestClientEnvFallback(t *testing.T) {
	srv, got := serveCapture(t)

	t.Setenv("DOCQL_API_KEY", "env-key")
	t.Setenv("DOCQL_API_URL", srv.URL+"/")
	client, err := NewClient()
	if err != nil {
		t.Fatalf("NewClient with only env: %v", err)
	}
	if client.cfg.apiKey != "env-key" || client.cfg.apiURL != srv.URL {
		t.Fatalf("cfg = key %q url %q, want the env values with the trailing slash stripped",
			client.cfg.apiKey, client.cfg.apiURL)
	}
	rec := sendProbe(t, client, got)
	if key := rec.header.Get("X-API-Key"); key != "env-key" {
		t.Fatalf("X-API-Key = %q, want the env key", key)
	}

	// Explicit options beat the environment.
	explicit, err := NewClient(WithAPIKey("explicit-key"), WithAPIURL("http://explicit.test/"))
	if err != nil {
		t.Fatalf("NewClient explicit: %v", err)
	}
	if explicit.cfg.apiKey != "explicit-key" || explicit.cfg.apiURL != "http://explicit.test" {
		t.Fatalf("cfg = key %q url %q, want the explicit values",
			explicit.cfg.apiKey, explicit.cfg.apiURL)
	}

	// An unset, empty or whitespace-only DOCQL_API_URL falls back to the
	// production default; the key still comes from the env.
	fallbacks := []struct {
		label string
		setup func()
	}{
		{"empty", func() { t.Setenv("DOCQL_API_URL", "") }},
		{"blank", func() { t.Setenv("DOCQL_API_URL", "   ") }},
		{"unset", func() {
			if err := os.Unsetenv("DOCQL_API_URL"); err != nil {
				t.Fatalf("unset DOCQL_API_URL: %v", err)
			}
		}},
	}
	for _, tc := range fallbacks {
		t.Run(tc.label, func(t *testing.T) {
			tc.setup()
			client, err := NewClient()
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			if client.cfg.apiURL != defaultAPIURL {
				t.Fatalf("apiURL = %q, want the default", client.cfg.apiURL)
			}
		})
	}
}

func TestClientMissingKey(t *testing.T) {
	// Neutralize any ambient DOCQL_API_KEY: every case here must fail to
	// resolve a key.
	t.Setenv("DOCQL_API_KEY", "")
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()

	cases := []struct {
		name   string
		envKey string
		envSet bool
		opts   []Option
	}{
		{name: "env-unset"},
		{name: "env-blank", envSet: true, envKey: "  "},
		{name: "explicit-empty", opts: []Option{WithAPIKey("")}},
		{name: "explicit-blank", opts: []Option{WithAPIKey("  ")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.envSet {
				t.Setenv("DOCQL_API_KEY", tc.envKey)
			}
			opts := append([]Option{WithAPIURL(srv.URL)}, tc.opts...)
			client, err := NewClient(opts...)
			if client != nil {
				t.Fatal("NewClient must return no client for a missing key")
			}
			if !errors.Is(err, ErrMissingAPIKey) {
				t.Fatalf("err = %v, want errors.Is(err, ErrMissingAPIKey)", err)
			}
			if !strings.Contains(err.Error(), "DOCQL_API_KEY") {
				t.Fatalf("err text %q must name DOCQL_API_KEY", err.Error())
			}
		})
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("server saw %d requests, want 0", n)
	}
}

func TestClientInvalidURL(t *testing.T) {
	for _, bad := range []string{"", "ftp://x", "api-docql.finoniq.com", "http://[::1", "http://"} {
		client, err := NewClient(WithAPIKey("k"), WithAPIURL(bad))
		if client != nil {
			t.Fatalf("NewClient(%q) returned a client, want none", bad)
		}
		if !errors.Is(err, ErrInvalidAPIURL) {
			t.Fatalf("NewClient(%q) err = %v, want errors.Is(err, ErrInvalidAPIURL)", bad, err)
		}
		if !strings.Contains(err.Error(), "DOCQL_API_URL") {
			t.Fatalf("err text %q must name DOCQL_API_URL", err.Error())
		}
	}
}

func TestClientInvalidTimeout(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		client, err := NewClient(WithAPIKey("k"), WithAPIURL("http://h"), WithTimeout(d))
		if client != nil {
			t.Fatalf("WithTimeout(%s) returned a client, want none", d)
		}
		if !errors.Is(err, ErrInvalidTimeout) {
			t.Fatalf("WithTimeout(%s) err = %v, want errors.Is(err, ErrInvalidTimeout)", d, err)
		}
	}

	// WithHTTPClient(nil) means "no injection": the SDK builds its own client.
	captureCall(t, []Option{WithAPIKey("k"), WithHTTPClient(nil)})
}

func TestClientDeadline(t *testing.T) {
	// A handler that sleeps 500 ms: every deadline variant shorter than that
	// must abort the call, the default must not.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()

	client, err := NewClient(WithAPIKey("k"), WithAPIURL(srv.URL), WithTimeout(50*time.Millisecond))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = client.QueryDocument(context.Background(),
		FileBytes([]byte("d"), "f.pdf"), Query("q"))
	if err == nil {
		t.Fatal("QueryDocument succeeded, want a timeout")
	}
	var ce *ConnectionError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %T (%v), want *ConnectionError", err, err)
	}
	if !ce.Timeout() {
		t.Fatal("Timeout() = false, want true for an expired deadline")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the chain to contain context.DeadlineExceeded", err)
	}

	// A caller deadline earlier than WithTimeout wins, and it fires quickly.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	client, err = NewClient(WithAPIKey("k"), WithAPIURL(srv.URL), WithTimeout(10*time.Second))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	start := time.Now()
	_, err = client.QueryDocument(ctx, FileBytes([]byte("d"), "f.pdf"), Query("q"))
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("call took %s, want the caller deadline to fire quickly", elapsed)
	}
	if !errors.As(err, &ce) || !ce.Timeout() || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a *ConnectionError timeout with DeadlineExceeded", err)
	}

	// Without WithTimeout the request context carries a deadline about 660 s
	// ahead, observed through a RoundTripper reading req.Context().
	deadlineSeen := make(chan time.Time, 1)
	rt := &deadlineRecorder{inner: http.DefaultTransport, out: deadlineSeen}
	hc := &http.Client{Transport: rt}
	client, err = NewClient(WithAPIKey("k"), WithAPIURL(srv.URL), WithHTTPClient(hc))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := client.QueryDocument(context.Background(),
			FileBytes([]byte("d"), "f.pdf"), Query("q"))
		done <- err
	}()
	select {
	case dl := <-deadlineSeen:
		delta := time.Until(dl)
		if delta <= 600*time.Second || delta > defaultTimeout {
			t.Fatalf("request deadline %s ahead, want about 660 s", delta)
		}
	case err := <-done:
		t.Fatalf("call ended before the deadline was observed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no request reached the RoundTripper")
	}
	if err := <-done; err != nil {
		t.Fatalf("QueryDocument: %v", err)
	}
}

// deadlineRecorder forwards to inner and reports the request-context deadline.
type deadlineRecorder struct {
	inner http.RoundTripper
	out   chan<- time.Time
	once  sync.Once
}

func (d *deadlineRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	dl, ok := req.Context().Deadline()
	if !ok {
		dl = time.Time{}
	}
	d.once.Do(func() { d.out <- dl })
	return d.inner.RoundTrip(req)
}

func TestClientInjectedTimeoutCleared(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()

	injected := &http.Client{Timeout: 50 * time.Millisecond, Transport: http.DefaultTransport}
	client, err := NewClient(WithAPIKey("k"), WithAPIURL(srv.URL), WithHTTPClient(injected))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	// The injected 50 ms default must not shorten a call the SDK deadline
	// governs; the handler's 200 ms sleep proves it.
	if _, err := client.QueryDocument(context.Background(),
		FileBytes([]byte("d"), "f.pdf"), Query("q")); err != nil {
		t.Fatalf("QueryDocument: %v", err)
	}
	if injected.Timeout != 50*time.Millisecond {
		t.Fatalf("injected Timeout = %s, want the caller's struct untouched", injected.Timeout)
	}
	if injected.CheckRedirect != nil {
		t.Fatal("injected CheckRedirect was replaced; want nil (untouched)")
	}
	if injected.Transport != http.DefaultTransport {
		t.Fatal("injected Transport pointer changed")
	}
}

func TestClientSDKTransport(t *testing.T) {
	tr := sdkTransport()
	htr, ok := tr.(*http.Transport)
	if !ok {
		t.Fatalf("sdkTransport is %T, want *http.Transport", tr)
	}
	if htr.TLSHandshakeTimeout != connectTimeout {
		t.Fatalf("TLSHandshakeTimeout = %s, want %s", htr.TLSHandshakeTimeout, connectTimeout)
	}
	if sdkDialer.Timeout != connectTimeout {
		t.Fatalf("sdkDialer.Timeout = %s, want %s", sdkDialer.Timeout, connectTimeout)
	}

	c1, err := NewClient(WithAPIKey("k"), WithAPIURL("http://h"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	c2, err := NewClient(WithAPIKey("k"), WithAPIURL("http://h"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c1.cfg.httpClient.Transport == nil || c1.cfg.httpClient.Transport != c2.cfg.httpClient.Transport {
		t.Fatal("two SDK-built clients must share one transport")
	}
}

func TestClientRedirectRefused(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer target.Close()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Location", target.URL+"/v1/query-document")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	run := func(label string, opts ...Option) {
		t.Helper()
		before := hits.Load()
		client, err := NewClient(append([]Option{WithAPIKey("redirect-test-key"), WithAPIURL(srv.URL)}, opts...)...)
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		_, err = client.QueryDocument(context.Background(),
			FileBytes([]byte("d"), "f.pdf"), Query("q"))
		var de *Error
		if !errors.As(err, &de) {
			t.Fatalf("%s: err = %T (%v), want *Error", label, err, err)
		}
		if de.Status != 307 {
			t.Fatalf("%s: status = %d, want 307", label, de.Status)
		}
		if de.Reason != "" {
			t.Fatalf("%s: reason = %q, want empty for a non-envelope 3xx", label, de.Reason)
		}
		if n := hits.Load(); n != before+1 {
			t.Fatalf("%s: first server went from %d to %d requests, want exactly one new request",
				label, before, n)
		}
		if n := targetHits.Load(); n != 0 {
			t.Fatalf("%s: the redirect target saw %d requests, want 0", label, n)
		}
	}

	run("sdk-client")
	// An injected client whose own CheckRedirect would follow: the SDK copy
	// must still refuse.
	follow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return nil
	}}
	run("injected-follower", WithHTTPClient(follow))
	if n := hits.Load(); n != 2 || targetHits.Load() != 0 {
		t.Fatalf("request counts after both runs: first server %d, target %d; want 2 and 0",
			hits.Load(), targetHits.Load())
	}
}

func TestClientHeaders(t *testing.T) {
	rec := captureCall(t, []Option{WithAPIKey("header-test-key")})

	if key := rec.header.Get("X-API-Key"); key != "header-test-key" {
		t.Fatalf("X-API-Key = %q, want the per-request key", key)
	}
	if ct := rec.header.Get("Content-Type"); !strings.HasPrefix(ct, "multipart/form-data; boundary=") {
		t.Fatalf("Content-Type = %q, want the multipart form", ct)
	}
	if rec.contentLength <= 0 {
		t.Fatalf("ContentLength = %d, want a known length", rec.contentLength)
	}
	ua := rec.header.Get("User-Agent")
	uaRe := regexp.MustCompile(`^docql-go/0\.1\.0 \([^;()]+; [^/()]+/[^()]+\)$`)
	if !uaRe.MatchString(ua) {
		t.Fatalf("User-Agent = %q, want the docql-go/0.1.0 (goVersion; goos/goarch) form", ua)
	}
	for _, want := range []string{runtime.Version(), runtime.GOOS, runtime.GOARCH} {
		if !strings.Contains(ua, want) {
			t.Fatalf("User-Agent %q must contain %q", ua, want)
		}
	}
	if rec.header.Get("X-Request-Id") != "" {
		t.Fatal("X-Request-Id must never be sent (D-15)")
	}
	for name := range rec.header {
		if strings.HasPrefix(strings.ToLower(name), "x-docql-") {
			t.Fatalf("header %s must never be sent (D-15)", name)
		}
	}
}

func TestClientStringMasksKey(t *testing.T) {
	const key = "dql_SECRETSECRETSECRETSECRETSECRET_abcdef"
	client, err := NewClient(WithAPIKey(key), WithAPIURL("http://h"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	deref := *client

	pointerForms := map[string]string{
		"%v":  fmt.Sprintf("%v", client),
		"%+v": fmt.Sprintf("%+v", client),
		"%#v": fmt.Sprintf("%#v", client),
		// The verb itself is under test: this must go through fmt, not the
		// String method, so the simplification S1025 suggests does not apply.
		"%s": fmt.Sprintf("%s", client), //lint:ignore S1025 the fmt verb is the behaviour under test
	}
	derefForms := map[string]string{
		"%v":  fmt.Sprintf("%v", deref),
		"%+v": fmt.Sprintf("%+v", deref),
		"%#v": fmt.Sprintf("%#v", deref),
		// any(deref) keeps vet's printf check quiet: Client has no value-
		// receiver String, which is exactly the behaviour under test — fmt
		// cannot reach the pointer method, so the key never prints.
		"%s": fmt.Sprintf("%s", any(deref)),
	}
	for verb, text := range pointerForms {
		if strings.Contains(text, key) {
			t.Fatalf("%s on *Client printed the key: %s", verb, text)
		}
	}
	for verb, text := range derefForms {
		if strings.Contains(text, key) {
			t.Fatalf("%s on a dereferenced Client printed the key: %s", verb, text)
		}
	}
	const want = `docql.Client{APIURL: "http://h"}`
	if got := client.String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
	if got := client.GoString(); got != want {
		t.Fatalf("GoString() = %q, want %q", got, want)
	}
	if !strings.Contains(pointerForms["%v"], "http://h") {
		t.Fatalf("String text must keep the API URL, got %q", pointerForms["%v"])
	}
}

func TestClientConcurrent(t *testing.T) {
	var mu sync.Mutex
	boundaries := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		boundaries[strings.TrimPrefix(r.Header.Get("Content-Type"), "multipart/form-data; boundary=")] = true
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{},"metadata":{"request_id":"c1"}}`))
	}))
	defer srv.Close()

	client, err := NewClient(WithAPIKey("k"), WithAPIURL(srv.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		go func() {
			_, err := client.QueryDocument(context.Background(),
				FileBytes([]byte("%PDF-1.4 concurrent"), "f.pdf"), Query("q"))
			errs <- err
		}()
	}
	for i := 0; i < 16; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent call: %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(boundaries) != 16 {
		t.Fatalf("server saw %d distinct boundaries, want 16", len(boundaries))
	}
}

func TestClientCanceled(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	client, err := NewClient(WithAPIKey("k"), WithAPIURL(srv.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := client.QueryDocument(ctx, FileBytes([]byte("d"), "f.pdf"), Query("q"))
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	err = <-done
	if err == nil {
		t.Fatal("QueryDocument succeeded after cancel, want an error")
	}
	var ce *ConnectionError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %T (%v), want *ConnectionError", err, err)
	}
	if ce.Timeout() {
		t.Fatal("Timeout() = true, want false for a cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the chain to contain context.Canceled", err)
	}
}

func TestClientModeOpen(t *testing.T) {
	srv, got := serveCapture(t)
	client, err := NewClient(WithAPIKey("k"), WithAPIURL(srv.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	rec := sendProbe(t, client, got, WithMode(Mode("turbo")))
	if got, want := bodyPart(t, rec)["params"], map[string]any{"mode": "turbo"}; !equalAny(got, want) {
		t.Fatalf("params = %v, want %v", got, want)
	}
	if !bytes.Contains(rec.body, []byte(`{"query":"q","params":{"mode":"turbo"}`)) {
		t.Fatalf("body part must carry the query first and then params.mode, got %q", rec.body)
	}

	// Without WithMode, params is omitted entirely.
	rec = sendProbe(t, client, got)
	if _, has := bodyPart(t, rec)["params"]; has {
		t.Fatalf("params present without WithMode: %v", bodyPart(t, rec))
	}
	if !bytes.Contains(rec.body, []byte(`{"query":"q"}`)) {
		t.Fatalf("body must be exactly the query part, got %q", rec.body)
	}

	if ModeFast != "fast" || ModeStandard != "standard" {
		t.Fatalf("mode constants = %q / %q, want fast / standard", ModeFast, ModeStandard)
	}
}

// equalAny compares two decoded JSON values structurally.
func equalAny(a, b any) bool {
	ab, aerr := json.Marshal(a)
	bb, berr := json.Marshal(b)
	return aerr == nil && berr == nil && bytes.Equal(ab, bb)
}
