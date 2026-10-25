package docql

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// The same-key submit retry (JSDK-02, D-07, D-08) under testing/synctest: the
// real time.NewTimer sleeps run on the bubble's fake clock, so the gaps
// between requests are exactly the sleeps. The answers come from the vendored
// kit's decodeCases. The behaviour list is docql-python's
// tests/unit/test_submit.py (26-06), so the two SDKs send the same requests
// on the same answers.

// submitCall is what the submit script saw of one request.
type submitCall struct {
	at   time.Time
	keys []string
	body []byte
}

// submitScript is a RoundTripper that reads and closes each request body and
// serves answers in order; with repeatLast the last answer repeats forever.
type submitScript struct {
	t          *testing.T
	answers    []scriptAnswer
	repeatLast bool

	mu    sync.Mutex
	calls []submitCall
}

func (s *submitScript) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
	}
	s.mu.Lock()
	i := len(s.calls)
	s.calls = append(s.calls, submitCall{at: time.Now(), keys: r.Header.Values("Idempotency-Key"), body: body})
	s.mu.Unlock()
	if i >= len(s.answers) {
		if !s.repeatLast {
			s.t.Errorf("request %d is past the end of the script", i+1)
			return nil, errors.New("script exhausted")
		}
		i = len(s.answers) - 1
	}
	a := s.answers[i]
	if a.block {
		<-r.Context().Done()
		return nil, r.Context().Err()
	}
	if a.err != nil {
		return nil, a.err
	}
	h := http.Header{}
	for k, v := range a.Headers {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: a.Status, Header: h, Body: io.NopCloser(strings.NewReader(a.BodyUTF8)), Request: r}, nil
}

func (s *submitScript) snapshot() []submitCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// gaps returns the fake-clock seconds between consecutive requests.
func (s *submitScript) gaps() []float64 {
	calls := s.snapshot()
	out := []float64{}
	for i := 1; i < len(calls); i++ {
		out = append(out, calls[i].at.Sub(calls[i-1].at).Seconds())
	}
	return out
}

// keysSent returns the Idempotency-Key lines of every request.
func (s *submitScript) keysSent() [][]string {
	var out [][]string
	for _, c := range s.snapshot() {
		out = append(out, c.keys)
	}
	return out
}

func (s *submitScript) client(t *testing.T) *Client {
	t.Helper()
	return transportClient(t, "dql_SECRETSECRETSECRETSECRETSECRET_abcdef", "https://api.example", s)
}

// submitAnswers are the kit answers the submit tests script.
type submitAnswers struct {
	queued, unavailable, unavailableBare, inProgress, conflict, queueFull scriptAnswer
}

func loadSubmitAnswers(t *testing.T) submitAnswers {
	t.Helper()
	kit := loadWaitKit(t)
	unavailable := kit.answer(t, "job-503-submit")
	if unavailable.Headers["retry-after"] != "5" {
		t.Fatalf("job-503-submit Retry-After = %q, want 5", unavailable.Headers["retry-after"])
	}
	return submitAnswers{
		queued:          kit.answer(t, "job-202-queued"),
		unavailable:     unavailable,
		unavailableBare: unavailable.withHeader("retry-after", ""),
		inProgress:      kit.answer(t, "job-409-idempotency-in-progress"),
		conflict:        kit.answer(t, "job-422-idempotency-key-conflict"),
		queueFull:       kit.answer(t, "job-429-queue-full-tenant"),
	}
}

// envelopeAnswer is a DocQL error envelope with optional headers.
func envelopeAnswer(status int, reason string, headers map[string]string) scriptAnswer {
	return scriptAnswer{Status: status, Headers: headers, BodyUTF8: envelope(status, reason)}
}

var submitPDF = []byte("%PDF-1.4 the same-key retry test document")

func submitOnce(c *Client, opts ...QueryOption) (*Job, error) {
	return c.SubmitDocument(context.Background(), FileBytes(submitPDF, "f.pdf"), Query("q"), opts...)
}

// asError returns err as a *docql.Error or fails the test.
func asError(t *testing.T, err error) *Error {
	t.Helper()
	var de *Error
	if !errors.As(err, &de) {
		t.Fatalf("got %T %v, want *docql.Error", err, err)
	}
	return de
}

func TestSubmitRetryConstants(t *testing.T) {
	if submitMaxAttempts != 4 {
		t.Errorf("submitMaxAttempts = %d, want 4", submitMaxAttempts)
	}
	if want := [...]time.Duration{time.Second, 2 * time.Second, 4 * time.Second}; submitBackoff != want {
		t.Errorf("submitBackoff = %v, want %v", submitBackoff, want)
	}
}

func TestSubmitRetryRule(t *testing.T) {
	a := loadSubmitAnswers(t)

	t.Run("503 then 202 returns the job with the same key on both attempts", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := &submitScript{t: t, answers: []scriptAnswer{a.unavailable, a.queued}}
			job, err := submitOnce(s.client(t), WithIdempotencyKey("order-1"))
			if err != nil {
				t.Fatalf("SubmitDocument: %v", err)
			}
			if job.Status != JobStatusQueued || job.IdempotencyKey != "order-1" || job.JobID == "" {
				t.Fatalf("got %q %q key %q, want the queued job with the key", job.JobID, job.Status, job.IdempotencyKey)
			}
			if got, want := s.keysSent(), [][]string{{"order-1"}, {"order-1"}}; !slices.EqualFunc(got, want, slices.Equal) {
				t.Errorf("keys %q, want %q", got, want)
			}
			if got := s.gaps(); !slices.Equal(got, []float64{5}) {
				t.Errorf("sleeps %v, want [5]", got)
			}
		})
	})

	t.Run("503 four times returns the fourth with the key", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := &submitScript{t: t, answers: []scriptAnswer{a.unavailable}, repeatLast: true}
			_, err := submitOnce(s.client(t), WithIdempotencyKey("order-2"))
			de := asError(t, err)
			if de.Status != 503 || de.Reason != ReasonUpstreamUnavailable || de.IdempotencyKey != "order-2" || de.RetryAfter != 5*time.Second {
				t.Fatalf("got %+v, want the 503 upstream_unavailable with the key and Retry-After 5", de)
			}
			if got := s.keysSent(); len(got) != 4 || !slices.EqualFunc(got, [][]string{{"order-2"}, {"order-2"}, {"order-2"}, {"order-2"}}, slices.Equal) {
				t.Errorf("keys %q, want order-2 on exactly 4 requests", got)
			}
			if got := s.gaps(); !slices.Equal(got, []float64{5, 5, 5}) {
				t.Errorf("sleeps %v, want [5 5 5]", got)
			}
		})
	})

	t.Run("503 without Retry-After backs off 1 2 4", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := &submitScript{t: t, answers: []scriptAnswer{a.unavailableBare}, repeatLast: true}
			_, err := submitOnce(s.client(t))
			if de := asError(t, err); de.Status != 503 {
				t.Fatalf("status %d, want 503", de.Status)
			}
			if got := s.gaps(); !slices.Equal(got, []float64{1, 2, 4}) {
				t.Errorf("sleeps %v, want [1 2 4]", got)
			}
		})
	})

	t.Run("any 503 is retried whatever its reason", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := &submitScript{t: t, answers: []scriptAnswer{envelopeAnswer(503, ReasonServiceOverloaded, nil), a.queued}}
			if _, err := submitOnce(s.client(t)); err != nil {
				t.Fatalf("SubmitDocument: %v", err)
			}
			if got := s.gaps(); !slices.Equal(got, []float64{1}) {
				t.Errorf("sleeps %v, want [1]", got)
			}
		})
	})

	for _, tc := range []struct {
		retryAfter string
		slept      float64
	}{{"0", 1}, {"1", 1}, {"7", 7}, {"30", 30}, {"3600", 30}, {"garbage", 1}} {
		t.Run("Retry-After "+tc.retryAfter, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := &submitScript{t: t, answers: []scriptAnswer{a.unavailable.withHeader("retry-after", tc.retryAfter), a.queued}}
				if _, err := submitOnce(s.client(t)); err != nil {
					t.Fatalf("SubmitDocument: %v", err)
				}
				if got := s.gaps(); !slices.Equal(got, []float64{tc.slept}) {
					t.Errorf("sleeps %v, want [%v]", got, tc.slept)
				}
			})
		})
	}

	t.Run("409 in progress then 202 is retried after Retry-After 1", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := &submitScript{t: t, answers: []scriptAnswer{a.inProgress, a.queued}}
			job, err := submitOnce(s.client(t), WithIdempotencyKey("order-3"))
			if err != nil || job.IdempotencyKey != "order-3" {
				t.Fatalf("got %v %v, want the job with the key", job, err)
			}
			if got, want := s.keysSent(), [][]string{{"order-3"}, {"order-3"}}; !slices.EqualFunc(got, want, slices.Equal) {
				t.Errorf("keys %q, want %q", got, want)
			}
			if got := s.gaps(); !slices.Equal(got, []float64{1}) {
				t.Errorf("sleeps %v, want [1]", got)
			}
		})
	})
}

func TestSubmitErrorsReturnedAfterOneRequest(t *testing.T) {
	a := loadSubmitAnswers(t)
	for _, tc := range []struct {
		name   string
		answer scriptAnswer
	}{
		{"422 idempotency_key_conflict", a.conflict},
		{"409 with another reason", envelopeAnswer(409, ReasonIdempotencyKeyConflict, nil)},
		{"429 queue_full", a.queueFull},
		{"429 rate_limit_exceeded", envelopeAnswer(429, ReasonRateLimitExceeded, map[string]string{"retry-after": "1"})},
		{"429 concurrency_limit_exceeded", envelopeAnswer(429, ReasonConcurrencyLimitExceeded, map[string]string{"retry-after": "1"})},
		{"400", envelopeAnswer(400, ReasonInvalidParams, nil)},
		{"401", envelopeAnswer(401, ReasonInvalidAPIKey, nil)},
		{"413", envelopeAnswer(413, ReasonUploadTooLarge, nil)},
		{"500", envelopeAnswer(500, "internal_error", nil)},
		{"502", envelopeAnswer(502, "bad_gateway", nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := &submitScript{t: t, answers: []scriptAnswer{tc.answer, a.queued}}
				start := time.Now()
				_, err := submitOnce(s.client(t), WithIdempotencyKey("order-5"))
				de := asError(t, err)
				if de.Status != tc.answer.Status || de.IdempotencyKey != "order-5" {
					t.Fatalf("got %d key %q, want %d with the key", de.Status, de.IdempotencyKey, tc.answer.Status)
				}
				if n := len(s.snapshot()); n != 1 {
					t.Errorf("%d requests, want 1", n)
				}
				if d := time.Since(start); d != 0 {
					t.Errorf("returned after %s, want at once", d)
				}
			})
		})
	}
}

func TestSubmitTransportFailures(t *testing.T) {
	a := loadSubmitAnswers(t)
	asConn := func(t *testing.T, err error) *ConnectionError {
		t.Helper()
		var ce *ConnectionError
		if !errors.As(err, &ce) {
			t.Fatalf("got %T %v, want *docql.ConnectionError", err, err)
		}
		return ce
	}

	t.Run("a connection error is returned with the key after one request", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := &submitScript{t: t, answers: []scriptAnswer{{err: errors.New("connection reset by peer")}, a.queued}}
			_, err := submitOnce(s.client(t), WithIdempotencyKey("order-6"))
			if ce := asConn(t, err); ce.IdempotencyKey != "order-6" || ce.Timeout() {
				t.Fatalf("got key %q timeout %v, want the key and no timeout", ce.IdempotencyKey, ce.Timeout())
			}
			if n := len(s.snapshot()); n != 1 {
				t.Errorf("%d requests, want 1", n)
			}
		})
	})

	t.Run("a timeout is returned with the key after one request", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := &submitScript{t: t, answers: []scriptAnswer{{block: true}, a.queued}}
			start := time.Now()
			_, err := submitOnce(s.client(t), WithIdempotencyKey("order-7"))
			if ce := asConn(t, err); ce.IdempotencyKey != "order-7" || !ce.Timeout() {
				t.Fatalf("got key %q timeout %v, want the key and a timeout", ce.IdempotencyKey, ce.Timeout())
			}
			if n := len(s.snapshot()); n != 1 {
				t.Errorf("%d requests, want 1", n)
			}
			if d := time.Since(start); d != defaultTimeout {
				t.Errorf("returned after %s, want the %s per-attempt deadline", d, defaultTimeout)
			}
		})
	})

	t.Run("a connection error after a 503 is returned with the same key", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := &submitScript{t: t, answers: []scriptAnswer{a.unavailable, {err: errors.New("connection refused")}}}
			_, err := submitOnce(s.client(t), WithIdempotencyKey("order-8"))
			if ce := asConn(t, err); ce.IdempotencyKey != "order-8" {
				t.Fatalf("key %q, want order-8", ce.IdempotencyKey)
			}
			if got, want := s.keysSent(), [][]string{{"order-8"}, {"order-8"}}; !slices.EqualFunc(got, want, slices.Equal) {
				t.Errorf("keys %q, want %q", got, want)
			}
		})
	})

	t.Run("an unusable 2xx carries the key", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := &submitScript{t: t, answers: []scriptAnswer{{Status: 202, BodyUTF8: "<html>"}}}
			_, err := submitOnce(s.client(t), WithIdempotencyKey("order-9"))
			if de := asError(t, err); de.IdempotencyKey != "order-9" {
				t.Fatalf("key %q, want order-9", de.IdempotencyKey)
			}
			if n := len(s.snapshot()); n != 1 {
				t.Errorf("%d requests, want 1", n)
			}
		})
	})

	t.Run("a cancel during a retry sleep returns at once with the key", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := &submitScript{t: t, answers: []scriptAnswer{a.unavailable.withHeader("retry-after", "30")}, repeatLast: true}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			time.AfterFunc(2*time.Second, cancel)
			start := time.Now()
			_, err := s.client(t).SubmitDocument(ctx, FileBytes(submitPDF, "f.pdf"), Query("q"), WithIdempotencyKey("order-10"))
			ce := asConn(t, err)
			if ce.IdempotencyKey != "order-10" || !errors.Is(err, context.Canceled) || ce.Timeout() {
				t.Fatalf("got %v key %q timeout %v, want context.Canceled wrapped with the key", err, ce.IdempotencyKey, ce.Timeout())
			}
			if strings.Contains(err.Error(), "order-10") {
				t.Errorf("Error() prints the key: %q", err.Error())
			}
			if d := time.Since(start); d != 2*time.Second {
				t.Errorf("returned after %s, want 2s: the 30 s timer must not be awaited", d)
			}
			if n := len(s.snapshot()); n != 1 {
				t.Errorf("%d requests, want 1", n)
			}
		})
	})

	t.Run("a ctx deadline during a retry sleep is a timeout with the key", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := &submitScript{t: t, answers: []scriptAnswer{a.unavailable}, repeatLast: true}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			start := time.Now()
			_, err := s.client(t).SubmitDocument(ctx, FileBytes(submitPDF, "f.pdf"), Query("q"), WithIdempotencyKey("order-11"))
			ce := asConn(t, err)
			if ce.IdempotencyKey != "order-11" || !errors.Is(err, context.DeadlineExceeded) || !ce.Timeout() {
				t.Fatalf("got %v key %q timeout %v, want DeadlineExceeded wrapped with the key", err, ce.IdempotencyKey, ce.Timeout())
			}
			if d := time.Since(start); d != 3*time.Second {
				t.Errorf("returned after %s, want 3s", d)
			}
			if n := len(s.snapshot()); n != 1 {
				t.Errorf("%d requests, want 1", n)
			}
		})
	})
}

// plainReader hides every method but Read, so the SDK cannot measure or
// rewind it.
type plainReader struct{ r io.Reader }

func (p plainReader) Read(b []byte) (int, error) { return p.r.Read(b) }

func TestSubmitReplayability(t *testing.T) {
	a := loadSubmitAnswers(t)
	data := []byte("0123456789 the document bytes after offset seven")

	t.Run("replayable inputs are retried with identical bodies", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "doc.pdf")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name string
			file func() File
			sent []byte // the file bytes that must be on the wire
		}{
			{"FileBytes", func() File { return FileBytes(data, "doc.pdf") }, data},
			{"FilePath", func() File { return FilePath(path) }, data},
			{"FileReader over a bytes.Reader at offset 7", func() File {
				r := bytes.NewReader(data)
				if _, err := r.Seek(7, io.SeekStart); err != nil {
					t.Fatal(err)
				}
				return FileReader(r, "doc.pdf")
			}, data[7:]},
		} {
			t.Run(tc.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					s := &submitScript{t: t, answers: []scriptAnswer{a.unavailable, a.inProgress, a.queued}}
					if _, err := s.client(t).SubmitDocument(context.Background(), tc.file(), Query("q")); err != nil {
						t.Fatalf("SubmitDocument: %v", err)
					}
					calls := s.snapshot()
					if len(calls) != 3 {
						t.Fatalf("%d requests, want 3", len(calls))
					}
					for i, c := range calls {
						if !bytes.Equal(c.body, calls[0].body) {
							t.Errorf("request %d body differs from the first", i+1)
						}
						if !bytes.Contains(c.body, tc.sent) {
							t.Errorf("request %d body lacks the file bytes", i+1)
						}
					}
					if len(tc.sent) != len(data) && bytes.Contains(calls[0].body, data) {
						t.Error("the reader was sent from its start, not from offset 7")
					}
				})
			})
		}
	})

	for _, tc := range []struct {
		name string
		file func(t *testing.T) File
	}{
		{"FileReader over an io.Pipe", func(t *testing.T) File {
			pr, pw := io.Pipe()
			go func() {
				_, _ = pw.Write(data)
				_ = pw.Close()
			}()
			return FileReader(pr, "doc.pdf")
		}},
		{"FileReader over a bytes.Buffer", func(*testing.T) File { return FileReader(bytes.NewBuffer(data), "doc.pdf") }},
		{"FileReader over a plain io.Reader", func(*testing.T) File { return FileReader(plainReader{bytes.NewReader(data)}, "doc.pdf") }},
	} {
		for _, first := range []struct {
			name   string
			answer scriptAnswer
		}{{"503", a.unavailable}, {"409 in progress", a.inProgress}} {
			t.Run(tc.name+" is sent once on a "+first.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					s := &submitScript{t: t, answers: []scriptAnswer{first.answer, a.queued}}
					_, err := s.client(t).SubmitDocument(context.Background(), tc.file(t), Query("q"), WithIdempotencyKey("order-12"))
					de := asError(t, err)
					if de.Status != first.answer.Status || de.IdempotencyKey != "order-12" {
						t.Fatalf("got %d key %q, want the first %d with the key", de.Status, de.IdempotencyKey, first.answer.Status)
					}
					if n := len(s.snapshot()); n != 1 {
						t.Errorf("%d requests, want 1", n)
					}
				})
			})
		}
	}

	t.Run("a path that vanished before the retry is a ConnectionError with the key", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "doc.pdf")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		synctest.Test(t, func(t *testing.T) {
			s := &submitScript{t: t, answers: []scriptAnswer{a.unavailable, a.queued}}
			orig := openFile
			opened := 0
			openFile = func(name string) (*os.File, error) {
				opened++
				if opened > 1 {
					return nil, os.ErrNotExist
				}
				return orig(name)
			}
			defer func() { openFile = orig }()
			_, err := s.client(t).SubmitDocument(context.Background(), FilePath(path), Query("q"), WithIdempotencyKey("order-13"))
			var ce *ConnectionError
			if !errors.As(err, &ce) || ce.IdempotencyKey != "order-13" || !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("got %T %v, want a *ConnectionError wrapping the open error with the key", err, err)
			}
			if n := len(s.snapshot()); n != 1 {
				t.Errorf("%d requests, want 1", n)
			}
		})
	})
}

func TestSubmitKeyRules(t *testing.T) {
	a := loadSubmitAnswers(t)

	t.Run("no key generates one rand.Text key reused on every attempt", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := &submitScript{t: t, answers: []scriptAnswer{a.unavailable, a.inProgress, a.queued}}
			job, err := submitOnce(s.client(t))
			if err != nil {
				t.Fatalf("SubmitDocument: %v", err)
			}
			sent := s.keysSent()
			if len(sent) != 3 || len(sent[0]) != 1 {
				t.Fatalf("keys %q, want one key on each of 3 requests", sent)
			}
			key := sent[0][0]
			if !slices.EqualFunc(sent, [][]string{{key}, {key}, {key}}, slices.Equal) {
				t.Errorf("keys %q, want %q on every attempt", sent, key)
			}
			if len(key) != 26 || strings.Trim(key, "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567") != "" {
				t.Errorf("key %q is not a 26-character rand.Text key", key)
			}
			if job.IdempotencyKey != key {
				t.Errorf("job key %q, want %q", job.IdempotencyKey, key)
			}
		})
	})

	t.Run("each call without a key gets a new key", func(t *testing.T) {
		s := &submitScript{t: t, answers: []scriptAnswer{a.queued}, repeatLast: true}
		c := s.client(t)
		first, err1 := submitOnce(c)
		second, err2 := submitOnce(c)
		if err1 != nil || err2 != nil {
			t.Fatalf("SubmitDocument: %v %v", err1, err2)
		}
		if first.IdempotencyKey == second.IdempotencyKey {
			t.Errorf("two calls sent the same generated key %q", first.IdempotencyKey)
		}
	})

	printable := make([]byte, 0, 0x7F-0x20)
	for b := byte(0x20); b < 0x7F; b++ {
		printable = append(printable, b)
	}
	for _, key := range []string{strings.Repeat("k", 255), " ", "~", " leading and trailing ", "a~b c", string(printable)} {
		t.Run(fmt.Sprintf("key of %d bytes sent unchanged", len(key)), func(t *testing.T) {
			s := &submitScript{t: t, answers: []scriptAnswer{a.queued}}
			job, err := submitOnce(s.client(t), WithIdempotencyKey(key))
			if err != nil {
				t.Fatalf("SubmitDocument: %v", err)
			}
			if got := s.keysSent(); len(got) != 1 || !slices.Equal(got[0], []string{key}) {
				t.Errorf("keys %q, want [[%q]]", got, key)
			}
			if job.IdempotencyKey != key {
				t.Errorf("job key %q, want %q", job.IdempotencyKey, key)
			}
		})
	}

	for _, key := range []string{"", strings.Repeat("k", 256), "a\x1fb", "a\x7fb", "café", "ก", "a\nb", "a\tb"} {
		t.Run(fmt.Sprintf("key %q refused before any I/O", key), func(t *testing.T) {
			s := &submitScript{t: t, answers: []scriptAnswer{a.queued}}
			orig := openFile
			opened := 0
			openFile = func(name string) (*os.File, error) { opened++; return orig(name) }
			defer func() { openFile = orig }()
			for _, file := range []File{FileBytes(submitPDF, "f.pdf"), FilePath(filepath.Join(t.TempDir(), "x.pdf"))} {
				_, err := s.client(t).SubmitDocument(context.Background(), file, Query("q"), WithIdempotencyKey(key))
				if !errors.Is(err, ErrInvalidIdempotencyKey) {
					t.Fatalf("got %v, want ErrInvalidIdempotencyKey", err)
				}
				if key != "" && strings.Contains(err.Error(), key) {
					t.Errorf("the error echoes the key: %q", err.Error())
				}
			}
			if n := len(s.snapshot()); n != 0 || opened != 0 {
				t.Errorf("%d requests and %d opens, want none", n, opened)
			}
		})
	}

	t.Run("exactly one Idempotency-Key line per request and never in Error()", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := &submitScript{t: t, answers: []scriptAnswer{a.unavailable}, repeatLast: true}
			_, err := submitOnce(s.client(t), WithIdempotencyKey("secret-order-key"))
			if strings.Contains(err.Error(), "secret-order-key") || strings.Contains(fmt.Sprintf("%+v", err), "secret-order-key") {
				t.Errorf("the error prints the key: %q", err.Error())
			}
			for i, keys := range s.keysSent() {
				if !slices.Equal(keys, []string{"secret-order-key"}) {
					t.Errorf("request %d carries Idempotency-Key lines %q, want exactly one", i+1, keys)
				}
			}
		})
	})
}
