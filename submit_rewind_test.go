package docql

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The same-key submit retry must not rewind a caller's seekable FileReader
// while the previous attempt's transport may still read it. net/http's
// RoundTripper contract lets the transport keep reading, and close, the
// request body after RoundTrip has returned an early answer; "callers wanting
// to reuse the body for subsequent requests must arrange to wait for the
// Close call before doing so". These tests use the real net/http transport
// and server over loopback: the server answers 503 with Retry-After 1 before
// it reads the upload, and the caller's reader stalls inside its second Read,
// the way a network-backed io.ReadSeeker can.

// stallSeeker is a caller's io.ReadSeeker whose first Read in the first
// attempt stalls for up to stall. It records a Seek that arrives while a Read
// is still in flight: the SDK rewinding the reader under the previous
// attempt's transport. Such a Seek also ends the stall, so the stalled Read
// then reads from the rewound position, as a real reader would.
//
// The first attempt is told apart by the absolute seeks: SubmitDocument's
// length measurement ends with one io.SeekStart (it restores the position),
// and each same-key retry rewinds with one more. Only the first attempt's
// first Read stalls, and a later attempt never stalls. stalling is closed
// when the stall begins, so the server can hold its early answer until the
// transport is inside the caller's Read: the overlap then never depends on
// how far the transport got before the answer arrived.
type stallSeeker struct {
	stall time.Duration

	mu      sync.Mutex
	r       *bytes.Reader
	starts  int // io.SeekStart calls so far
	stalled bool

	inRead   atomic.Int32
	overlap  atomic.Bool
	stalling chan struct{}
	seeked   chan struct{}
	seekOnce sync.Once
}

func newStallSeeker(data []byte, stall time.Duration) *stallSeeker {
	return &stallSeeker{stall: stall, r: bytes.NewReader(data), stalling: make(chan struct{}), seeked: make(chan struct{})}
}

func (s *stallSeeker) Read(p []byte) (int, error) {
	s.inRead.Add(1)
	defer s.inRead.Add(-1)
	s.mu.Lock()
	stall := !s.stalled && s.starts <= 1
	s.stalled = s.stalled || stall
	s.mu.Unlock()
	if stall {
		close(s.stalling)
		t := time.NewTimer(s.stall)
		select {
		case <-s.seeked:
		case <-t.C:
		}
		t.Stop()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.r.Read(p)
}

func (s *stallSeeker) Seek(offset int64, whence int) (int64, error) {
	if s.inRead.Load() > 0 {
		s.overlap.Store(true)
		s.seekOnce.Do(func() { close(s.seeked) })
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if whence == io.SeekStart {
		s.starts++
	}
	return s.r.Seek(offset, whence)
}

// earlyAnswerServer answers the first request 503 upstream_unavailable with
// Retry-After 1 without reading its body, once src has started its stall,
// and every later one with the kit's 202 queued job after reading the whole
// upload. It records the file part of each later request.
type earlyAnswerServer struct {
	*httptest.Server
	requests atomic.Int32
	mu       sync.Mutex
	files    [][]byte
}

func newEarlyAnswerServer(t *testing.T, src *stallSeeker) *earlyAnswerServer {
	t.Helper()
	queued := loadSubmitAnswers(t).queued
	s := &earlyAnswerServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.requests.Add(1) == 1 {
			select {
			case <-src.stalling:
			case <-time.After(5 * time.Second):
				t.Error("the first attempt never read the caller's reader")
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, envelope(http.StatusServiceUnavailable, ReasonUpstreamUnavailable))
			return
		}
		var file []byte
		if mr, err := r.MultipartReader(); err == nil {
			if p, err := mr.NextPart(); err == nil && p.FormName() == "file" {
				file, _ = io.ReadAll(p)
			}
		}
		_, _ = io.Copy(io.Discard, r.Body)
		s.mu.Lock()
		s.files = append(s.files, file)
		s.mu.Unlock()
		for k, v := range queued.Headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(queued.Status)
		_, _ = io.WriteString(w, queued.BodyUTF8)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *earlyAnswerServer) filesSeen() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.files...)
}

// rewindTestData is large enough that the upload cannot finish in one Read.
func rewindTestData() []byte {
	data := make([]byte, 1<<20)
	for i := range data {
		data[i] = byte('a' + i%26)
	}
	return data
}

func TestSubmitRetryWaitsForThePreviousBodyBeforeRewinding(t *testing.T) {
	data := rewindTestData()
	src := newStallSeeker(data, 3*time.Second)
	srv := newEarlyAnswerServer(t, src)
	c := newInputClient(t, srv.URL)

	start := time.Now()
	job, err := c.SubmitDocument(context.Background(), FileReader(src, "big.pdf"), Query("q"), WithIdempotencyKey("rewind-key"))
	elapsed := time.Since(start)

	if src.overlap.Load() {
		t.Errorf("the retry rewound the FileReader while the previous attempt's transport was still reading it (after %v)", elapsed.Round(time.Millisecond))
	}
	if err != nil {
		t.Fatalf("SubmitDocument: %v", err)
	}
	if job.IdempotencyKey != "rewind-key" {
		t.Errorf("Job.IdempotencyKey = %q, want rewind-key", job.IdempotencyKey)
	}
	if got := srv.requests.Load(); got != 2 {
		t.Errorf("requests = %d, want 2 (503, then the same-key retry)", got)
	}
	files := srv.filesSeen()
	if len(files) != 1 || !bytes.Equal(files[0], data) {
		got := -1
		if len(files) == 1 {
			got = len(files[0])
		}
		t.Errorf("the retry's file part is not the caller's bytes from the start offset (got %d bytes, want %d)", got, len(data))
	}
}

func TestSubmitRetryWaitForThePreviousBodyEndsWithCtx(t *testing.T) {
	src := newStallSeeker(rewindTestData(), 3*time.Second)
	srv := newEarlyAnswerServer(t, src)
	c := newInputClient(t, srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	job, err := c.SubmitDocument(ctx, FileReader(src, "big.pdf"), Query("q"), WithIdempotencyKey("rewind-key"))
	elapsed := time.Since(start)

	if src.overlap.Load() {
		t.Errorf("the retry rewound the FileReader while the previous attempt's transport was still reading it (after %v)", elapsed.Round(time.Millisecond))
	}
	if err == nil {
		t.Fatalf("SubmitDocument returned job %+v, want the ctx deadline while waiting for the previous upload", job)
	}
	var ce *ConnectionError
	if !errors.As(err, &ce) {
		t.Fatalf("got %T %v, want *docql.ConnectionError", err, err)
	}
	if ce.IdempotencyKey != "rewind-key" {
		t.Errorf("ConnectionError.IdempotencyKey = %q, want rewind-key", ce.IdempotencyKey)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("errors.Is(err, context.DeadlineExceeded) = false for %v", err)
	}
	if got := srv.requests.Load(); got != 1 {
		t.Errorf("requests = %d, want 1 (no retry once ctx has ended)", got)
	}
	if elapsed > 2900*time.Millisecond {
		t.Errorf("returned after %v, want about 2 s (the ctx deadline)", elapsed.Round(time.Millisecond))
	}
}
