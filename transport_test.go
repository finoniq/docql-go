package docql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// roundTripFunc adapts a function into an http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// transportClient builds a client whose calls run through fn.
func transportClient(t *testing.T, key, apiURL string, fn http.RoundTripper) *Client {
	t.Helper()
	c, err := NewClient(WithAPIKey(key), WithAPIURL(apiURL), WithHTTPClient(&http.Client{Transport: fn}))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// probeCall sends one small upload through the client.
func probeCall(ctx context.Context, c *Client) (*QueryResult, error) {
	return c.QueryDocument(ctx, FileBytes([]byte("d"), "f.pdf"), Query("q"))
}

// errReader yields an error instead of bytes.
type errReader struct{ err error }

func (r *errReader) Read([]byte) (int, error) { return 0, r.err }

// jsonEnvelope answers like the stub's sendEnvelopeError with one reason.
func jsonEnvelope(w http.ResponseWriter, status int, reason, errorInfo, requestID string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"error_info":%q,"status_code":%d,"metadata":{"reason":%q,"request_id":%q}}`,
		errorInfo, status, reason, requestID)
}

// TestTransportMapping pins the D-10 rule: every transport failure — refused
// connect, a connection closed mid-response, a body read that fails half-way,
// a DNS failure — becomes *ConnectionError, never a raw *url.Error.
func TestTransportMapping(t *testing.T) {
	build := func(apiURL string, fn http.RoundTripper) *Client {
		if fn == nil {
			c, err := NewClient(WithAPIKey("k"), WithAPIURL(apiURL))
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			return c
		}
		return transportClient(t, "k", apiURL, fn)
	}

	// A closed localhost port: connection refused.
	ln, lerr := net.Listen("tcp", "127.0.0.1:0")
	if lerr != nil {
		t.Fatalf("listen: %v", lerr)
	}
	closedAddr := ln.Addr().String()
	ln.Close()

	// A server that reads the request, then hijacks and closes the
	// connection without answering.
	hijack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
	}))
	defer hijack.Close()

	// A 2xx whose body read fails half-way.
	brokenBody := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			Status:     "200 OK",
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(io.MultiReader(
				strings.NewReader(`{"data": `),
				&errReader{err: errors.New("read failed mid-body")},
			)),
		}, nil
	})

	scenarios := map[string]struct {
		apiURL string
		fn     http.RoundTripper
	}{
		"closed-port":  {apiURL: "http://" + closedAddr, fn: nil},
		"hijacked":     {apiURL: hijack.URL, fn: nil},
		"broken-body":  {apiURL: "http://stub.test", fn: brokenBody},
		"invalid-host": {apiURL: "http://docql.test.invalid", fn: nil},
	}
	for name, sc := range scenarios {
		t.Run(name, func(t *testing.T) {
			_, err := probeCall(context.Background(), build(sc.apiURL, sc.fn))
			if err == nil {
				t.Fatal("call succeeded, want a transport failure")
			}
			var ce *ConnectionError
			if !errors.As(err, &ce) {
				t.Fatalf("err = %T (%v), want *ConnectionError", err, err)
			}
			if ce.Timeout() {
				t.Error("Timeout() = true, want false for a plain transport failure")
			}
			if ce.Unwrap() == nil {
				t.Error("Unwrap() = nil, want the cause")
			}
			if _, isURLErr := err.(*url.Error); isURLErr {
				t.Errorf("err is a raw *url.Error at the top level: %v", err)
			}
		})
	}
}

// TestTransportNoRetry pins the paid-call rule: nothing is retried, so a 429,
// a 503 and an expired deadline each reach the transport exactly once.
func TestTransportNoRetry(t *testing.T) {
	for _, status := range []int{429, 503} {
		var calls int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&calls, 1)
			_, _ = io.Copy(io.Discard, r.Body)
			jsonEnvelope(w, status, "service_overloaded", "busy", "r-1")
		}))
		client, cerr := NewClient(WithAPIKey("k"), WithAPIURL(srv.URL))
		if cerr != nil {
			t.Fatalf("NewClient: %v", cerr)
		}
		_, err := probeCall(context.Background(), client)
		var de *Error
		if !errors.As(err, &de) || de.Status != status {
			t.Fatalf("status %d: err = %v, want *Error(%d)", status, err, status)
		}
		if n := atomic.LoadInt32(&calls); n != 1 {
			t.Fatalf("status %d: transport reached %d times, want exactly 1", status, n)
		}
		srv.Close()
	}

	// An expired deadline aborts after one attempt too.
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = io.Copy(io.Discard, r.Body)
		time.Sleep(400 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	client, cerr := NewClient(WithAPIKey("k"), WithAPIURL(srv.URL), WithTimeout(50*time.Millisecond))
	if cerr != nil {
		t.Fatalf("NewClient: %v", cerr)
	}
	_, err := probeCall(context.Background(), client)
	var ce *ConnectionError
	if !errors.As(err, &ce) || !ce.Timeout() {
		t.Fatalf("err = %v, want a *ConnectionError timeout", err)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("timeout path reached the transport %d times, want exactly 1", n)
	}
}

// countingBody answers with 10 MiB while counting what was actually pulled
// and whether Close ran.
type countingBody struct {
	r      io.Reader
	pulled int
	closed bool
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.pulled += n
	return n, err
}

func (b *countingBody) Close() error { b.closed = true; return nil }

// TestTransportBoundedRead pins the DoS bound (T-12-15): a 10 MiB error body
// is read at most 65536 bytes, closed, and kept at 2048 characters; a large
// success body is read in full.
func TestTransportBoundedRead(t *testing.T) {
	body := &countingBody{r: io.LimitReader(strings.NewReader(strings.Repeat("x", 10<<20)), 10<<20)}
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			Status:     "500 Internal Server Error",
			StatusCode: http.StatusInternalServerError,
			Header:     http.Header{"Content-Type": []string{"text/plain"}},
			Body:       body,
		}, nil
	})
	_, err := probeCall(context.Background(), transportClient(t, "k", "http://stub.test", rt))
	var de *Error
	if !errors.As(err, &de) || de.Status != 500 {
		t.Fatalf("err = %v, want *Error(500)", err)
	}
	if body.pulled > 65536 {
		t.Errorf("pulled %d bytes from the wire, want at most 65536", body.pulled)
	}
	if !body.closed {
		t.Error("the response body was never closed")
	}
	if n := len([]rune(de.Body)); n > 2048 {
		t.Errorf("Body keeps %d characters, want at most 2048", n)
	}

	// A 1 MiB success body is read in full.
	blob := strings.Repeat("y", 1<<20)
	payload, _ := json.Marshal(map[string]any{
		"data":     map[string]any{"blob": blob},
		"metadata": map[string]any{"request_id": "r-big"},
	})
	full := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			Status:     "200 OK",
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(payload)),
		}, nil
	})
	res, err := probeCall(context.Background(), transportClient(t, "k", "http://stub.test", full))
	if err != nil {
		t.Fatalf("1 MiB success body: %v", err)
	}
	var data struct {
		Blob string `json:"blob"`
	}
	if uerr := json.Unmarshal(res.Data, &data); uerr != nil {
		t.Fatalf("decode data: %v", uerr)
	}
	if len(data.Blob) != 1<<20 {
		t.Errorf("blob length = %d, want the full 1 MiB", len(data.Blob))
	}
}

// TestTransportSizeHint pins the D-10 hint: a known upload over the 25 MiB
// ceiling names the size in the connection message; a small upload does not;
// neither failure is a *Error.
func TestTransportSizeHint(t *testing.T) {
	failing := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused by test")
	})
	client := transportClient(t, "k", "http://stub.test", failing)

	big := make([]byte, 26214401)
	_, err := client.QueryDocument(context.Background(), FileBytes(big, "big.pdf"), Query("q"))
	var ce *ConnectionError
	if !errors.As(err, &ce) {
		t.Fatalf("big upload: err = %T (%v), want *ConnectionError", err, err)
	}
	if !strings.Contains(ce.Error(), "25 MiB") {
		t.Errorf("big upload message carries no size hint: %q", ce.Error())
	}
	var de *Error
	if errors.As(err, &de) {
		t.Error("big upload: err is a *Error")
	}

	_, err = client.QueryDocument(context.Background(), FileBytes([]byte("0123456789"), "small.pdf"), Query("q"))
	if !errors.As(err, &ce) {
		t.Fatalf("small upload: err = %T (%v), want *ConnectionError", err, err)
	}
	if strings.Contains(ce.Error(), "25 MiB") {
		t.Errorf("small upload message wrongly carries a size hint: %q", ce.Error())
	}
	if errors.As(err, &de) {
		t.Error("small upload: err is a *Error")
	}
}

// TestTransportKeyRedaction pins the D-13 privacy rule: a server that echoes
// the X-API-Key back — into an envelope error_info, a plain body, or not at
// all — never puts the key on any surface the SDK controls.
func TestTransportKeyRedaction(t *testing.T) {
	key := "dql_SECRETSECRETSECRETSECRETSECRET_abcdef"

	echoEnvelope := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		echoed := r.Header.Get("X-API-Key")
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"error_info":"key %s rejected","status_code":401,"metadata":{"reason":"invalid_api_key","request_id":"r-401"}}`, echoed)
	})
	echoPlain := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		echoed := r.Header.Get("X-API-Key")
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, "bad key %s", echoed)
	})

	call := func(handler http.Handler) error {
		srv := httptest.NewServer(handler)
		defer srv.Close()
		client, cerr := NewClient(WithAPIKey(key), WithAPIURL(srv.URL))
		if cerr != nil {
			t.Fatalf("NewClient: %v", cerr)
		}
		_, err := probeCall(context.Background(), client)
		if err == nil {
			t.Fatal("call succeeded, want an error")
		}
		return err
	}

	envelopeErr := call(echoEnvelope)
	plainErr := call(echoPlain)

	refusedClient, cerr := NewClient(WithAPIKey(key), WithAPIURL("http://127.0.0.1:1"))
	if cerr != nil {
		t.Fatalf("NewClient: %v", cerr)
	}
	_, refusedErr := probeCall(context.Background(), refusedClient)
	if refusedErr == nil {
		t.Fatal("refused call succeeded, want an error")
	}

	surfaces := func(err error) map[string]string {
		var de *Error
		m := map[string]string{
			"Error()": err.Error(),
			"%v":      fmt.Sprintf("%v", err),
			"%+v":     fmt.Sprintf("%+v", err),
		}
		if errors.As(err, &de) {
			m["Body"] = de.Body
			m["Message"] = de.Message
		}
		return m
	}
	for name, err := range map[string]error{
		"envelope-401":    envelopeErr,
		"plain-400":       plainErr,
		"refused-connect": refusedErr,
	} {
		for surface, text := range surfaces(err) {
			if strings.Contains(text, key) {
				t.Errorf("%s: %s contains the key: %q", name, surface, text)
			}
		}
	}

	if s := refusedClient.String(); strings.Contains(s, key) {
		t.Errorf("String() contains the key: %q", s)
	}
	if s := refusedClient.GoString(); strings.Contains(s, key) {
		t.Errorf("GoString() contains the key: %q", s)
	}
}
