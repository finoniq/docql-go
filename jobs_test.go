package docql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// jobKitBody returns the body of one job-cases.json decode case.
func jobKitBody(t *testing.T, id string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("contract", "job-cases.json"))
	if err != nil {
		t.Fatalf("read contract/job-cases.json: %v", err)
	}
	var kit struct {
		DecodeCases []jobDecodeCase `json:"decodeCases"`
	}
	if err := json.Unmarshal(raw, &kit); err != nil {
		t.Fatalf("decode contract/job-cases.json: %v", err)
	}
	for _, c := range kit.DecodeCases {
		if c.ID == id {
			return c.BodyUTF8
		}
	}
	t.Fatalf("job decode case %q not found", id)
	return ""
}

// wireRequest is what the test server saw of one request.
type wireRequest struct {
	method     string
	requestURI string
}

// jobServer answers every request with status and body and records each
// request's method and raw request URI.
type jobServer struct {
	*httptest.Server
	mu   sync.Mutex
	seen []wireRequest
}

func newJobServer(t *testing.T, status int, body string) *jobServer {
	t.Helper()
	s := &jobServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		s.mu.Lock()
		s.seen = append(s.seen, wireRequest{method: r.Method, requestURI: r.RequestURI})
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req-test")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *jobServer) requests() []wireRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.seen)
}

func (s *jobServer) client(t *testing.T) *Client {
	t.Helper()
	c, err := NewClient(WithAPIKey("k"), WithAPIURL(s.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

const testJobID = "job_0123456789ABCDEFGHIJKL"

func envelope(status int, reason string) string {
	return fmt.Sprintf(`{"error_info":"e","status_code":%d,"metadata":{"reason":%q,"request_id":"r"}}`, status, reason)
}

func TestGetJobFailedReturnsTheJob(t *testing.T) {
	srv := newJobServer(t, 200, jobKitBody(t, "job-200-failed-upstream"))
	job, err := srv.client(t).GetJob(context.Background(), testJobID)
	if err != nil {
		t.Fatalf("GetJob on a failed job returned an error: %v", err)
	}
	if job.Status != JobStatusFailed || job.Error == nil {
		t.Fatalf("got status %q error %v, want failed with its Error", job.Status, job.Error)
	}
	if job.Error.Reason != ReasonUpstreamUnavailable || job.Error.Retryable || job.Error.JobID != testJobID {
		t.Fatalf("Error = %+v, want upstream_unavailable, not retryable, with the job id", job.Error)
	}
	if n := len(srv.requests()); n != 1 {
		t.Fatalf("%d requests, want 1", n)
	}
}

func TestGetJobErrorsAfterOneRequest(t *testing.T) {
	for _, tc := range []struct {
		status    int
		reason    string
		retryable bool
	}{
		{404, ReasonJobNotFound, false},
		{503, ReasonUpstreamUnavailable, true},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			srv := newJobServer(t, tc.status, envelope(tc.status, tc.reason))
			job, err := srv.client(t).GetJob(context.Background(), testJobID)
			if job != nil {
				t.Fatalf("got a job %+v, want nil", job)
			}
			var de *Error
			if !errors.As(err, &de) {
				t.Fatalf("got %T %v, want *docql.Error", err, err)
			}
			if de.Status != tc.status || de.Reason != tc.reason || de.Retryable != tc.retryable || de.JobID != testJobID {
				t.Fatalf("got %+v", de)
			}
			if n := len(srv.requests()); n != 1 {
				t.Fatalf("%d requests, want 1", n)
			}
		})
	}
}

func TestGetJobKeepsAnUnknownStatus(t *testing.T) {
	srv := newJobServer(t, 200, `{"job_id":"j1","status":"paused","metadata":{}}`)
	job, err := srv.client(t).GetJob(context.Background(), "j1")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Status != "paused" {
		t.Fatalf("Status = %q, want paused kept verbatim", job.Status)
	}
}

func TestCancelJob202SendsOneDelete(t *testing.T) {
	srv := newJobServer(t, 202, jobKitBody(t, "job-delete-202-cancelled"))
	job, err := srv.client(t).CancelJob(context.Background(), testJobID)
	if err != nil {
		t.Fatalf("CancelJob: %v", err)
	}
	if job.Status != JobStatusCancelled || job.JobID != testJobID {
		t.Fatalf("got %q %q, want the cancelled job", job.JobID, job.Status)
	}
	want := []wireRequest{{http.MethodDelete, "/v1/jobs/" + testJobID}}
	if got := srv.requests(); !slices.Equal(got, want) {
		t.Fatalf("requests %v, want %v", got, want)
	}
}

func TestCancelJobOnATerminalJobIsRepeatable(t *testing.T) {
	srv := newJobServer(t, 200, jobKitBody(t, "job-delete-200-terminal"))
	c := srv.client(t)
	first, err := c.CancelJob(context.Background(), testJobID)
	if err != nil {
		t.Fatalf("first CancelJob: %v", err)
	}
	second, err := c.CancelJob(context.Background(), testJobID)
	if err != nil {
		t.Fatalf("second CancelJob: %v", err)
	}
	if first.JobID != second.JobID || first.Status != second.Status {
		t.Fatalf("first %q %q, second %q %q, want the same terminal job", first.JobID, first.Status, second.JobID, second.Status)
	}
	if first.Status == JobStatusQueued || first.Status == JobStatusRunning {
		t.Fatalf("a 200 cancel answered a non-terminal status %q", first.Status)
	}
	for _, r := range srv.requests() {
		if r.method != http.MethodDelete {
			t.Fatalf("CancelJob sent %s", r.method)
		}
	}
	if n := len(srv.requests()); n != 2 {
		t.Fatalf("%d requests, want 2", n)
	}
}

func TestCancelJob404CarriesTheJobID(t *testing.T) {
	srv := newJobServer(t, 404, envelope(404, ReasonJobNotFound))
	_, err := srv.client(t).CancelJob(context.Background(), testJobID)
	var de *Error
	if !errors.As(err, &de) || de.Reason != ReasonJobNotFound || de.JobID != testJobID {
		t.Fatalf("got %T %v, want 404 job_not_found with the job id", err, err)
	}
}

func TestQueryDocumentRefusesWithIdempotencyKey(t *testing.T) {
	srv := newJobServer(t, 200, `{"data":{}}`)
	_, err := srv.client(t).QueryDocument(context.Background(),
		FileBytes([]byte("d"), "f.pdf"), Query("q"), WithIdempotencyKey("k-1"))
	if !errors.Is(err, ErrIdempotencyKeyOnQuery) {
		t.Fatalf("got %v, want ErrIdempotencyKeyOnQuery", err)
	}
	if n := len(srv.requests()); n != 0 {
		t.Fatalf("%d requests, want none", n)
	}
}

// TestJobHeaderHygiene inspects each request as the SDK built it, before any
// transport adds to it: submit carries exactly X-API-Key, User-Agent,
// Content-Type and one Idempotency-Key; poll and cancel carry exactly
// X-API-Key and User-Agent. No X-Request-Id or x-docql-* header is ever set.
func TestJobHeaderHygiene(t *testing.T) {
	var mu sync.Mutex
	built := map[string]http.Header{}
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		built[r.Method] = r.Header.Clone()
		mu.Unlock()
		status, body := 200, jobKitBody(t, "job-200-running")
		switch r.Method {
		case http.MethodPost:
			status, body = 202, jobKitBody(t, "job-202-queued")
		case http.MethodDelete:
			status, body = 202, jobKitBody(t, "job-delete-202-cancelled")
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})
	c := transportClient(t, "k", "https://api.example", rt)
	ctx := context.Background()
	if _, err := c.SubmitDocument(ctx, FileBytes([]byte("d"), "f.pdf"), Query("q")); err != nil {
		t.Fatalf("SubmitDocument: %v", err)
	}
	if _, err := c.GetJob(ctx, testJobID); err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if _, err := c.CancelJob(ctx, testJobID); err != nil {
		t.Fatalf("CancelJob: %v", err)
	}

	names := func(h http.Header) []string {
		var out []string
		for k := range h {
			out = append(out, k)
		}
		slices.Sort(out)
		return out
	}
	want := map[string][]string{
		http.MethodPost:   {"Content-Type", "Idempotency-Key", "User-Agent", "X-Api-Key"},
		http.MethodGet:    {"User-Agent", "X-Api-Key"},
		http.MethodDelete: {"User-Agent", "X-Api-Key"},
	}
	for method, w := range want {
		h, ok := built[method]
		if !ok {
			t.Fatalf("no %s request was sent", method)
		}
		if got := names(h); !slices.Equal(got, w) {
			t.Errorf("%s headers %v, want exactly %v", method, got, w)
		}
	}
	if keys := built[http.MethodPost].Values("Idempotency-Key"); len(keys) != 1 || keys[0] == "" {
		t.Errorf("submit Idempotency-Key lines %q, want exactly one", keys)
	}
}

func TestJobRedirectIsDecodedAsAnError(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	c, err := NewClient(WithAPIKey("k"), WithAPIURL(srv.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = c.GetJob(context.Background(), testJobID)
	var de *Error
	if !errors.As(err, &de) || de.Status != http.StatusTemporaryRedirect || de.JobID != testJobID {
		t.Fatalf("got %T %v, want a 307 *docql.Error with the job id", err, err)
	}
	if hits != 1 {
		t.Fatalf("%d requests, want 1: the redirect must not be followed", hits)
	}
}

func TestJobPathOnTheWire(t *testing.T) {
	for id, want := range map[string]string{
		"..":  "/v1/jobs/%2E%2E",
		".":   "/v1/jobs/%2E",
		"a.b": "/v1/jobs/a%2Eb",
		"a/b": "/v1/jobs/a%2Fb",
		"a b": "/v1/jobs/a%20b",
	} {
		t.Run(id, func(t *testing.T) {
			srv := newJobServer(t, 200, `{"job_id":"x","status":"running"}`)
			if _, err := srv.client(t).GetJob(context.Background(), id); err != nil {
				t.Fatalf("GetJob: %v", err)
			}
			got := srv.requests()
			if len(got) != 1 || got[0].requestURI != want {
				t.Fatalf("request URIs %v, want [%s]", got, want)
			}
		})
	}
}

func TestJobMethodsRefuseBadArgumentsBeforeAnyRequest(t *testing.T) {
	srv := newJobServer(t, 200, `{"job_id":"x","status":"running"}`)
	c := srv.client(t)
	ctx := context.Background()
	var nilCtx context.Context
	calls := map[string]func(*Client, context.Context, string) error{
		"GetJob":    func(c *Client, ctx context.Context, id string) error { _, err := c.GetJob(ctx, id); return err },
		"Wait":      func(c *Client, ctx context.Context, id string) error { _, err := c.Wait(ctx, id); return err },
		"CancelJob": func(c *Client, ctx context.Context, id string) error { _, err := c.CancelJob(ctx, id); return err },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if err := call(c, ctx, ""); !errors.Is(err, ErrNoJobID) {
				t.Errorf("empty id: %v, want ErrNoJobID", err)
			}
			if err := call(c, nilCtx, testJobID); !errors.Is(err, ErrNilContext) {
				t.Errorf("nil ctx: %v, want ErrNilContext", err)
			}
			var nilClient *Client
			if err := call(nilClient, ctx, testJobID); !errors.Is(err, ErrNilClient) {
				t.Errorf("nil client: %v, want ErrNilClient", err)
			}
			if err := call(&Client{}, ctx, testJobID); !errors.Is(err, ErrNilClient) {
				t.Errorf("zero client: %v, want ErrNilClient", err)
			}
		})
	}
	var nilClient *Client
	if _, err := nilClient.SubmitDocument(ctx, FileBytes([]byte("d"), "f.pdf"), Query("q")); !errors.Is(err, ErrNilClient) {
		t.Errorf("SubmitDocument on a nil client: %v, want ErrNilClient", err)
	}
	if _, err := c.SubmitDocument(nilCtx, FileBytes([]byte("d"), "f.pdf"), Query("q")); !errors.Is(err, ErrNilContext) {
		t.Errorf("SubmitDocument with a nil ctx: %v, want ErrNilContext", err)
	}
	if n := len(srv.requests()); n != 0 {
		t.Fatalf("%d requests, want none", n)
	}
}

func TestSubmitDocumentKeys(t *testing.T) {
	ctx := context.Background()
	file := FileBytes([]byte("d"), "f.pdf")

	t.Run("generated key on the job", func(t *testing.T) {
		var sent []string
		rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			sent = r.Header.Values("Idempotency-Key")
			return &http.Response{StatusCode: 202, Header: http.Header{},
				Body: io.NopCloser(strings.NewReader(jobKitBody(t, "job-202-queued"))), Request: r}, nil
		})
		job, err := transportClient(t, "k", "https://api.example", rt).SubmitDocument(ctx, file, Query("q"))
		if err != nil {
			t.Fatalf("SubmitDocument: %v", err)
		}
		if len(sent) != 1 || job.IdempotencyKey != sent[0] || validateIdempotencyKey(sent[0]) != nil {
			t.Fatalf("sent %q, job key %q: want the one valid key sent on the job", sent, job.IdempotencyKey)
		}
		if job.Status != JobStatusQueued || job.JobID == "" {
			t.Fatalf("got %q %q, want a queued job", job.JobID, job.Status)
		}
	})

	t.Run("caller key unchanged and on errors", func(t *testing.T) {
		const key = "caller key 1"
		var sent []string
		rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			sent = r.Header.Values("Idempotency-Key")
			return &http.Response{StatusCode: 422, Header: http.Header{},
				Body: io.NopCloser(strings.NewReader(envelope(422, ReasonIdempotencyKeyConflict))), Request: r}, nil
		})
		_, err := transportClient(t, "k", "https://api.example", rt).SubmitDocument(ctx, file, Query("q"), WithIdempotencyKey(key))
		var de *Error
		if !errors.As(err, &de) || de.IdempotencyKey != key {
			t.Fatalf("got %T %v, want *docql.Error carrying the key", err, err)
		}
		if strings.Contains(de.Error(), key) {
			t.Fatalf("Error() prints the idempotency key: %q", de.Error())
		}
		if !slices.Equal(sent, []string{key}) {
			t.Fatalf("sent %q, want [%q]", sent, key)
		}
	})

	t.Run("connection error carries the key", func(t *testing.T) {
		const key = "caller-key-2"
		rt := roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("connection reset") })
		_, err := transportClient(t, "k", "https://api.example", rt).SubmitDocument(ctx, file, Query("q"), WithIdempotencyKey(key))
		var ce *ConnectionError
		if !errors.As(err, &ce) || ce.IdempotencyKey != key {
			t.Fatalf("got %T %v, want *docql.ConnectionError carrying the key", err, err)
		}
		if strings.Contains(ce.Error(), key) {
			t.Fatalf("Error() prints the idempotency key: %q", ce.Error())
		}
	})

	t.Run("bad keys refused before sending", func(t *testing.T) {
		srv := newJobServer(t, 202, jobKitBody(t, "job-202-queued"))
		c := srv.client(t)
		for _, key := range []string{"", strings.Repeat("a", 256), "k\x7f", "k\ney", "kกey"} {
			_, err := c.SubmitDocument(ctx, file, Query("q"), WithIdempotencyKey(key))
			if !errors.Is(err, ErrInvalidIdempotencyKey) {
				t.Errorf("key %q: %v, want ErrInvalidIdempotencyKey", key, err)
			}
			if err != nil && key != "" && strings.Contains(err.Error(), key) {
				t.Errorf("the error echoes the key: %q", err.Error())
			}
		}
		if _, err := c.SubmitDocument(ctx, file, Query("q"), WithIdempotencyKey(strings.Repeat("a", 255))); err != nil {
			t.Errorf("a 255-character key: %v, want accepted", err)
		}
		if n := len(srv.requests()); n != 1 {
			t.Fatalf("%d requests, want only the valid key's", n)
		}
	})
}

// TestErrorJobFields pins the Error() forms with the job fields: job_id is
// added only when set, and the idempotency key never prints.
func TestErrorJobFields(t *testing.T) {
	e := &Error{Status: 404, Reason: "job_not_found", Message: "m", RequestID: "r", JobID: "j1", IdempotencyKey: "secret-key"}
	if got, want := e.Error(), "404 job_not_found: m (request_id=r, job_id=j1)"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	framework := &Error{Status: 404, Body: `{"detail":"Not Found"}`, JobID: "j1", IdempotencyKey: "secret-key"}
	if got, want := framework.Error(), `404 (non-DocQL response): {"detail":"Not Found"} (job_id=j1)`; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	hostile := &Error{Status: 404, Reason: "job_not_found", JobID: "j\r\nforged"}
	if strings.ContainsAny(hostile.Error(), "\r\n") {
		t.Errorf("Error() carries a raw CR or LF from the job id: %q", hostile.Error())
	}
	ce := &ConnectionError{msg: "docql: connection failed", IdempotencyKey: "secret-key"}
	if strings.Contains(ce.Error(), "secret-key") {
		t.Errorf("ConnectionError.Error() prints the key")
	}

	we := &WaitError{JobID: "j1", err: context.DeadlineExceeded}
	if got, want := we.Error(), "docql: wait for job j1 stopped: context deadline exceeded; the job was not cancelled"; got != want {
		t.Errorf("WaitError.Error() = %q, want %q", got, want)
	}
	if !we.Timeout() || !errors.Is(we, context.DeadlineExceeded) {
		t.Error("a deadline WaitError must report Timeout() and wrap context.DeadlineExceeded")
	}
	cancelled := &WaitError{JobID: "j1", err: context.Canceled}
	if cancelled.Timeout() || !errors.Is(cancelled, context.Canceled) {
		t.Error("a cancelled WaitError must not report Timeout() and must wrap context.Canceled")
	}
}
