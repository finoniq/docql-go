package docql

import (
	"bytes"
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// WR-03 (26-REVIEW): an injected http.RoundTripper that reads the request
// body but never closes it must not make the same-key submit retry block
// forever. net/http's own transport closes every request body, but test
// doubles, recorders and hand-written RoundTrippers accepted through
// WithHTTPClient often do not. These tests use only context.Background(), as
// the README example does, so nothing but the SDK itself can end the call.

// openBodyScript is a RoundTripper that reads each request body fully and
// never closes it, then serves its answers in order; the last answer repeats.
// It records the Idempotency-Key lines and the file part of every request.
type openBodyScript struct {
	answers []scriptAnswer

	mu    sync.Mutex
	keys  [][]string
	files [][]byte
	// answered counts the answers served, so a seeker can tell a Seek made
	// after request 1 was answered from the length measurement before it.
	answered int
}

func (s *openBodyScript) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body) // read, never closed
	}
	s.mu.Lock()
	i := len(s.keys)
	s.keys = append(s.keys, r.Header.Values("Idempotency-Key"))
	s.files = append(s.files, multipartFile(r.Header.Get("Content-Type"), body))
	if i >= len(s.answers) {
		i = len(s.answers) - 1
	}
	s.answered++
	s.mu.Unlock()
	a := s.answers[i]
	h := http.Header{}
	for k, v := range a.Headers {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: a.Status, Header: h, Body: io.NopCloser(strings.NewReader(a.BodyUTF8)), Request: r}, nil
}

// multipartFile returns the bytes of the "file" part of a multipart body, or nil.
func multipartFile(contentType string, body []byte) []byte {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil
	}
	mr := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		p, err := mr.NextPart()
		if err != nil {
			return nil
		}
		if p.FormName() == "file" {
			b, _ := io.ReadAll(p)
			return b
		}
	}
}

func (s *openBodyScript) keysSent() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]string(nil), s.keys...)
}

func (s *openBodyScript) filesSent() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.files...)
}

func (s *openBodyScript) answeredSoFar() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.answered
}

func (s *openBodyScript) client(t *testing.T, opts ...Option) *Client {
	t.Helper()
	c, err := NewClient(append([]Option{
		WithAPIKey("dql_SECRETSECRETSECRETSECRETSECRET_abcdef"),
		WithAPIURL("https://api.example"),
		WithHTTPClient(&http.Client{Transport: s}),
	}, opts...)...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// submitResult is what one SubmitDocument call returned.
type submitResult struct {
	job     *Job
	err     error
	elapsed time.Duration
}

// submitWithWatchdog runs submit in a goroutine and fails the test when it
// has not returned after 10 s of real time.
func submitWithWatchdog(t *testing.T, submit func() (*Job, error)) submitResult {
	t.Helper()
	done := make(chan submitResult, 1)
	start := time.Now()
	go func() {
		job, err := submit()
		done <- submitResult{job: job, err: err, elapsed: time.Since(start)}
	}()
	watchdog := time.NewTimer(10 * time.Second)
	defer watchdog.Stop()
	select {
	case r := <-done:
		return r
	case <-watchdog.C:
		t.Fatalf("SubmitDocument still blocked after 10s")
		return submitResult{}
	}
}

// releaseTestData is the document the non-closing transport tests upload.
var releaseTestData = []byte("%PDF-1.4 the WR-03 non-closing transport document")

// checkRetriedJob asserts the queued job came back after exactly two
// requests that both carried the key of request 1.
func checkRetriedJob(t *testing.T, s *openBodyScript, r submitResult) {
	t.Helper()
	if r.err != nil {
		t.Fatalf("SubmitDocument: %v (after %v)", r.err, r.elapsed.Round(time.Millisecond))
	}
	keys := s.keysSent()
	if len(keys) != 2 {
		t.Fatalf("requests = %d, want 2 (503, then the same-key retry)", len(keys))
	}
	if len(keys[0]) != 1 || keys[0][0] == "" {
		t.Fatalf("request 1 Idempotency-Key lines = %q, want one key", keys[0])
	}
	if len(keys[1]) != 1 || keys[1][0] != keys[0][0] {
		t.Errorf("request 2 Idempotency-Key lines = %q, want the key of request 1", keys[1])
	}
	if r.job.Status != JobStatusQueued || r.job.JobID == "" {
		t.Errorf("got job %q %q, want the queued job", r.job.JobID, r.job.Status)
	}
	if r.job.IdempotencyKey != keys[0][0] {
		t.Errorf("Job.IdempotencyKey is not the key sent on request 1")
	}
	files := s.filesSent()
	if !bytes.Equal(files[1], releaseTestData) {
		t.Errorf("the retry's file part is %d bytes, want the %d bytes of the input", len(files[1]), len(releaseTestData))
	}
}

func TestSubmitRetryFileBytesNonClosingTransport(t *testing.T) {
	a := loadSubmitAnswers(t)
	s := &openBodyScript{answers: []scriptAnswer{a.unavailable.withHeader("retry-after", "1"), a.queued}}
	c := s.client(t)

	r := submitWithWatchdog(t, func() (*Job, error) {
		return c.SubmitDocument(context.Background(), FileBytes(releaseTestData, "f.pdf"), Query("q"))
	})
	checkRetriedJob(t, s, r)
}

func TestSubmitRetryFilePathNonClosingTransport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "doc.pdf")
	if err := os.WriteFile(path, releaseTestData, 0o600); err != nil {
		t.Fatal(err)
	}
	a := loadSubmitAnswers(t)
	s := &openBodyScript{answers: []scriptAnswer{a.unavailable.withHeader("retry-after", "1"), a.queued}}
	c := s.client(t)

	r := submitWithWatchdog(t, func() (*Job, error) {
		return c.SubmitDocument(context.Background(), FilePath(path), Query("q"))
	})
	checkRetriedJob(t, s, r)
}
