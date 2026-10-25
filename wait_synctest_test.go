package docql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
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

// The wait tests run Wait's real code (time.NewTimer, context deadlines,
// time.Until) inside a testing/synctest bubble, whose fake clock advances
// only when every goroutine in the bubble is blocked. There is no clock seam
// in the production code (26-RESEARCH Pattern 5 item 3). The scripted answers
// come from the vendored kit (waitCases and decodeCases), never re-typed.

// scriptAnswer is one scripted answer: an HTTP response, or a transport
// error, or (block) a server that never answers until the request's context
// ends.
type scriptAnswer struct {
	Status   int               `json:"status"`
	Headers  map[string]string `json:"headers"`
	BodyUTF8 string            `json:"bodyUtf8"`
	err      error
	block    bool
}

// withHeader returns a copy of a with one header set, or removed when value
// is "".
func (a scriptAnswer) withHeader(name, value string) scriptAnswer {
	h := maps.Clone(a.Headers)
	if h == nil {
		h = map[string]string{}
	}
	delete(h, name)
	if value != "" {
		h[name] = value
	}
	a.Headers = h
	return a
}

// scriptCall is what the scripted transport saw of one request.
type scriptCall struct {
	method      string
	at          time.Time
	deadline    time.Time
	hasDeadline bool
}

// waitScript is a RoundTripper serving answers in order; with repeatLast the
// last answer repeats forever, else running past the end fails the test.
type waitScript struct {
	t          *testing.T
	answers    []scriptAnswer
	repeatLast bool

	mu    sync.Mutex
	calls []scriptCall
}

func (s *waitScript) RoundTrip(r *http.Request) (*http.Response, error) {
	dl, ok := r.Context().Deadline()
	s.mu.Lock()
	i := len(s.calls)
	s.calls = append(s.calls, scriptCall{method: r.Method, at: time.Now(), deadline: dl, hasDeadline: ok})
	s.mu.Unlock()
	if i >= len(s.answers) {
		if !s.repeatLast {
			s.t.Errorf("request %d (%s) is past the end of the script", i+1, r.Method)
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
	return &http.Response{
		StatusCode: a.Status,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(a.BodyUTF8)),
		Request:    r,
	}, nil
}

func (s *waitScript) snapshot() []scriptCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// gaps returns the fake-clock seconds between consecutive requests: the
// transport answers at once, so each gap is exactly one sleep.
func (s *waitScript) gaps() []float64 {
	calls := s.snapshot()
	var out []float64
	for i := 1; i < len(calls); i++ {
		out = append(out, calls[i].at.Sub(calls[i-1].at).Seconds())
	}
	return out
}

// check asserts what every wait must satisfy: only GET requests, and each
// poll's context deadline at most 30 s after the request started.
func (s *waitScript) check(t *testing.T) {
	t.Helper()
	for i, c := range s.snapshot() {
		if c.method != http.MethodGet {
			t.Errorf("request %d is %s: Wait only ever sends GET", i+1, c.method)
		}
		if !c.hasDeadline {
			t.Errorf("request %d has no deadline", i+1)
		} else if d := c.deadline.Sub(c.at); d > pollTimeout {
			t.Errorf("request %d deadline is %s after it started, want at most %s", i+1, d, pollTimeout)
		}
	}
}

func (s *waitScript) client(t *testing.T) *Client {
	t.Helper()
	// A key of the real dql_ shape: a toy key such as "k" is a substring of
	// reasons like invalid_api_key and would be scrubbed out of them.
	return transportClient(t, "dql_SECRETSECRETSECRETSECRETSECRET_abcdef", "https://api.example", s)
}

type waitKitCase struct {
	ID         string         `json:"id"`
	JobID      string         `json:"jobId"`
	Responses  []scriptAnswer `json:"responses"`
	RepeatLast bool           `json:"repeatLast"`
	Expect     struct {
		Outcome           string    `json:"outcome"`
		Reason            *string   `json:"reason"`
		SleepsSeconds     []float64 `json:"sleepsSeconds"`
		MinElapsedSeconds *float64  `json:"minElapsedSeconds"`
		Deletes           int       `json:"deletes"`
	} `json:"expect"`
}

type waitKit struct {
	WaitCases   []waitKitCase `json:"waitCases"`
	DecodeCases []struct {
		ID string `json:"id"`
		scriptAnswer
	} `json:"decodeCases"`
}

func loadWaitKit(t *testing.T) *waitKit {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("contract", "job-cases.json"))
	if err != nil {
		t.Fatalf("read contract/job-cases.json: %v", err)
	}
	var kit waitKit
	if err := json.Unmarshal(raw, &kit); err != nil {
		t.Fatalf("decode contract/job-cases.json: %v", err)
	}
	return &kit
}

func (k *waitKit) wait(t *testing.T, id string) waitKitCase {
	t.Helper()
	for _, w := range k.WaitCases {
		if w.ID == id {
			return w
		}
	}
	t.Fatalf("wait case %q not found", id)
	return waitKitCase{}
}

// answer returns a kit decode case as a scripted answer.
func (k *waitKit) answer(t *testing.T, id string) scriptAnswer {
	t.Helper()
	for _, c := range k.DecodeCases {
		if c.ID == id {
			return c.scriptAnswer
		}
	}
	t.Fatalf("decode case %q not found", id)
	return scriptAnswer{}
}

// waitOutcomeOf names a Wait result the way the kit's waitForJob does.
func waitOutcomeOf(t *testing.T, job *Job, err error, last scriptAnswer) (outcome, reason string) {
	t.Helper()
	if err == nil {
		return string(job.Status), ""
	}
	var we *WaitError
	if errors.As(err, &we) {
		return "timeout", ""
	}
	var de *Error
	if !errors.As(err, &de) {
		t.Fatalf("Wait returned %T: %v", err, err)
	}
	if last.Status == http.StatusOK && strings.Contains(last.BodyUTF8, `"status":"failed"`) {
		if de.Retryable {
			t.Error("a failed job's error is retryable")
		}
		return "failed", de.Reason
	}
	return "error", de.Reason
}

// TestWaitCasesSynctest replays the kit's waitCases W1-W7 through Wait on
// the bubble's fake clock: the outcome, the reason, the exact sleeps, the
// elapsed time and zero non-GET requests are the kit's.
func TestWaitCasesSynctest(t *testing.T) {
	kit := loadWaitKit(t)
	ids := make([]string, 0, len(kit.WaitCases))
	for _, w := range kit.WaitCases {
		ids = append(ids, w.ID)
	}
	want := []string{"w1-succeeded", "w2-failed", "w3-cancelled", "w4-expired", "w5-not-found", "w6-poll-429", "w7-timeout"}
	if !slices.Equal(ids, want) {
		t.Fatalf("the kit's waitCases are %v, want %v", ids, want)
	}
	for _, w := range kit.WaitCases {
		t.Run(w.ID, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := &waitScript{t: t, answers: w.Responses, repeatLast: w.RepeatLast}
				start := time.Now()
				job, err := s.client(t).Wait(context.Background(), w.JobID)
				elapsed := time.Since(start).Seconds()
				calls := s.snapshot()
				last := w.Responses[min(len(calls), len(w.Responses))-1]

				outcome, reason := waitOutcomeOf(t, job, err, last)
				if outcome != w.Expect.Outcome {
					t.Fatalf("outcome %s (%v), want %s", outcome, err, w.Expect.Outcome)
				}
				wantReason := ""
				if w.Expect.Reason != nil {
					wantReason = *w.Expect.Reason
				}
				if reason != wantReason {
					t.Errorf("reason %q, want %q", reason, wantReason)
				}
				if job != nil && job.JobID != w.JobID {
					t.Errorf("job id %q, want %q", job.JobID, w.JobID)
				}
				var de *Error
				if errors.As(err, &de) && de.JobID != w.JobID {
					t.Errorf("Error.JobID = %q, want %q", de.JobID, w.JobID)
				}
				if w.Expect.SleepsSeconds != nil && !slices.Equal(s.gaps(), w.Expect.SleepsSeconds) {
					t.Errorf("sleeps %v, want %v", s.gaps(), w.Expect.SleepsSeconds)
				}
				if w.Expect.MinElapsedSeconds != nil && elapsed < *w.Expect.MinElapsedSeconds {
					t.Errorf("elapsed %gs, want at least %gs", elapsed, *w.Expect.MinElapsedSeconds)
				}
				if outcome == "timeout" {
					var we *WaitError
					if !errors.As(err, &we) || !we.Timeout() || we.JobID != w.JobID {
						t.Errorf("got %v, want a *WaitError with Timeout() and the job id", err)
					}
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Errorf("errors.Is(err, context.DeadlineExceeded) = false: %v", err)
					}
					if elapsed != defaultWaitTimeout.Seconds() {
						t.Errorf("elapsed %gs, want exactly the %gs default deadline", elapsed, defaultWaitTimeout.Seconds())
					}
					for i, c := range calls {
						if !c.at.Before(start.Add(defaultWaitTimeout)) {
							t.Errorf("request %d ran at %s, at or past the deadline", i+1, c.at.Sub(start))
						}
					}
				}
				if w.Expect.Deletes != 0 {
					t.Fatalf("the kit expects %d deletes; Wait never deletes", w.Expect.Deletes)
				}
				s.check(t)
			})
		})
	}
}

// TestWaitW1WithoutRetryAfter is the kit's own variant (job-wait.test.ts): W1
// with every Retry-After removed sleeps 1 s each time.
func TestWaitW1WithoutRetryAfter(t *testing.T) {
	kit := loadWaitKit(t)
	w1 := kit.wait(t, "w1-succeeded")
	var answers []scriptAnswer
	for _, a := range w1.Responses {
		answers = append(answers, a.withHeader("retry-after", ""))
	}
	synctest.Test(t, func(t *testing.T) {
		s := &waitScript{t: t, answers: answers}
		job, err := s.client(t).Wait(context.Background(), w1.JobID)
		if err != nil || job.Status != JobStatusSucceeded {
			t.Fatalf("got %v %v, want succeeded", job, err)
		}
		if got := s.gaps(); !slices.Equal(got, []float64{1, 1}) {
			t.Fatalf("sleeps %v, want [1 1]", got)
		}
		s.check(t)
	})
}

// running is the kit's W1 running answer (Retry-After 2); succeeded its
// terminal answer.
func runningAndSucceeded(t *testing.T, kit *waitKit) (running, succeeded scriptAnswer) {
	t.Helper()
	w1 := kit.wait(t, "w1-succeeded")
	return w1.Responses[1], w1.Responses[2]
}

func TestWaitCallerDeadline(t *testing.T) {
	kit := loadWaitKit(t)
	running, _ := runningAndSucceeded(t, kit)
	synctest.Test(t, func(t *testing.T) {
		s := &waitScript{t: t, answers: []scriptAnswer{running}, repeatLast: true}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		start := time.Now()
		_, err := s.client(t).Wait(ctx, testJobID)
		var we *WaitError
		if !errors.As(err, &we) || we.JobID != testJobID {
			t.Fatalf("got %T %v, want *WaitError with the job id", err, err)
		}
		if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ctx.Err()) || !we.Timeout() {
			t.Fatalf("err %v must wrap context.DeadlineExceeded and ctx.Err() (%v)", err, ctx.Err())
		}
		if elapsed := time.Since(start); elapsed != 5*time.Second {
			t.Errorf("returned after %s, want exactly at the 5 s deadline", elapsed)
		}
		if got := s.gaps(); !slices.Equal(got, []float64{2, 2}) {
			t.Errorf("sleeps %v, want [2 2]: the third sleep would pass the deadline", got)
		}
		// The last poll (at 4 s) is capped by the wait's deadline, not 30 s.
		calls := s.snapshot()
		if last := calls[len(calls)-1]; !last.deadline.Equal(start.Add(5 * time.Second)) {
			t.Errorf("the last poll's deadline is %s after the start, want 5s", last.deadline.Sub(start))
		}
		s.check(t)
	})
}

func TestWaitCallerCancelDuringASleep(t *testing.T) {
	kit := loadWaitKit(t)
	running, _ := runningAndSucceeded(t, kit)
	synctest.Test(t, func(t *testing.T) {
		s := &waitScript{t: t, answers: []scriptAnswer{running.withHeader("retry-after", "30")}, repeatLast: true}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		time.AfterFunc(time.Second, cancel)
		start := time.Now()
		_, err := s.client(t).Wait(ctx, testJobID)
		var we *WaitError
		if !errors.As(err, &we) || we.JobID != testJobID {
			t.Fatalf("got %T %v, want *WaitError with the job id", err, err)
		}
		if !errors.Is(err, context.Canceled) || we.Timeout() {
			t.Fatalf("err %v must wrap context.Canceled and not report Timeout()", err)
		}
		if elapsed := time.Since(start); elapsed != time.Second {
			t.Errorf("returned after %s, want 1s: the 30 s timer must not be awaited", elapsed)
		}
		if n := len(s.snapshot()); n != 1 {
			t.Errorf("%d requests, want 1", n)
		}
		s.check(t)
	})
}

func TestWaitPollInterval(t *testing.T) {
	kit := loadWaitKit(t)
	running, succeeded := runningAndSucceeded(t, kit)
	for _, tc := range []struct {
		interval time.Duration
		want     float64
	}{
		{10 * time.Second, 10}, // raises the Retry-After of 2 s
		{time.Second, 2},       // never lowers it
		{0, 2},                 // ignored
		{-time.Second, 2},      // ignored
	} {
		t.Run(tc.interval.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := &waitScript{t: t, answers: []scriptAnswer{running, running, succeeded}}
				if _, err := s.client(t).Wait(context.Background(), testJobID, WithPollInterval(tc.interval)); err != nil {
					t.Fatalf("Wait: %v", err)
				}
				if got := s.gaps(); !slices.Equal(got, []float64{tc.want, tc.want}) {
					t.Errorf("sleeps %v, want [%g %g]", got, tc.want, tc.want)
				}
				s.check(t)
			})
		})
	}
}

func TestWaitRetryAfterClamp(t *testing.T) {
	kit := loadWaitKit(t)
	running, succeeded := runningAndSucceeded(t, kit)
	for _, tc := range []struct {
		name, value string
		want        float64
	}{
		{"zero", "0", 1},
		{"absent", "", 1},
		{"over the cap", "100", 30},
		{"in range", "7", 7},
		{"http-date", "+5s", 5},
		{"overflow", "99999999999999999999", 30},
		{"garbage", "soon", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				value := tc.value
				if value == "+5s" {
					value = time.Now().Add(5 * time.Second).UTC().Format(http.TimeFormat)
				}
				s := &waitScript{t: t, answers: []scriptAnswer{running.withHeader("retry-after", value), succeeded}}
				if _, err := s.client(t).Wait(context.Background(), testJobID); err != nil {
					t.Fatalf("Wait: %v", err)
				}
				if got := s.gaps(); !slices.Equal(got, []float64{tc.want}) {
					t.Errorf("sleeps %v, want [%g]", got, tc.want)
				}
				s.check(t)
			})
		})
	}
}

func TestWaitTransientBackoff(t *testing.T) {
	kit := loadWaitKit(t)
	running, succeeded := runningAndSucceeded(t, kit)
	poll503 := kit.answer(t, "job-503-poll")
	if _, ok := poll503.Headers["retry-after"]; ok {
		t.Fatal("job-503-poll carries Retry-After; the backoff test needs it absent")
	}
	runningNoHeader := running.withHeader("retry-after", "")
	rateLimited := kit.answer(t, "job-429-queue-full-tenant").withHeader("retry-after", "3")
	for _, tc := range []struct {
		name    string
		answers []scriptAnswer
		want    []float64
	}{
		{"503 without Retry-After backs off 1, 2, 4",
			[]scriptAnswer{poll503, poll503, poll503, succeeded}, []float64{1, 2, 4}},
		{"backoff caps at 30 and resets after a 200",
			append(slices.Repeat([]scriptAnswer{poll503}, 7), runningNoHeader, poll503, succeeded),
			[]float64{1, 2, 4, 8, 16, 30, 30, 1, 1}},
		{"a 429 with Retry-After is honoured",
			[]scriptAnswer{rateLimited, succeeded}, []float64{3}},
		{"network errors are retried",
			[]scriptAnswer{{err: errors.New("connection refused")}, {err: errors.New("connection reset")}, succeeded},
			[]float64{1, 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := &waitScript{t: t, answers: tc.answers}
				job, err := s.client(t).Wait(context.Background(), testJobID)
				if err != nil || job.Status != JobStatusSucceeded {
					t.Fatalf("got %v %v, want succeeded", job, err)
				}
				if got := s.gaps(); !slices.Equal(got, tc.want) {
					t.Errorf("sleeps %v, want %v", got, tc.want)
				}
				s.check(t)
			})
		})
	}
}

// TestWaitPerPollTimeout: a poll whose server never answers ends at its own
// 30 s deadline, counts as transient, and the wait goes on.
func TestWaitPerPollTimeout(t *testing.T) {
	kit := loadWaitKit(t)
	_, succeeded := runningAndSucceeded(t, kit)
	synctest.Test(t, func(t *testing.T) {
		s := &waitScript{t: t, answers: []scriptAnswer{{block: true}, succeeded}}
		start := time.Now()
		job, err := s.client(t).Wait(context.Background(), testJobID)
		if err != nil || job.Status != JobStatusSucceeded {
			t.Fatalf("got %v %v, want succeeded after the timed-out poll", job, err)
		}
		calls := s.snapshot()
		if len(calls) != 2 {
			t.Fatalf("%d requests, want 2", len(calls))
		}
		if d := calls[0].deadline.Sub(start); d != pollTimeout {
			t.Errorf("the first poll's deadline is %s, want %s", d, pollTimeout)
		}
		if at := calls[1].at.Sub(start); at != pollTimeout+time.Second {
			t.Errorf("the second poll ran at %s, want 31s (30 s timeout, then a 1 s backoff)", at)
		}
		s.check(t)
	})
}

func TestWaitErrorsReturnAtOnce(t *testing.T) {
	kit := loadWaitKit(t)
	_, succeeded := runningAndSucceeded(t, kit)
	unauthorized := scriptAnswer{
		Status:   401,
		Headers:  map[string]string{"content-type": "application/json", "x-request-id": "req-401"},
		BodyUTF8: `{"error_info":"invalid API key","metadata":{"reason":"invalid_api_key"}}`,
	}
	for _, tc := range []struct {
		name      string
		answer    scriptAnswer
		status    int
		reason    string
		retryable bool
	}{
		{"401", unauthorized, 401, ReasonInvalidAPIKey, false},
		{"404-not-found", kit.answer(t, "job-404-not-found"), 404, ReasonJobNotFound, false},
		{"404-expired", kit.answer(t, "job-404-expired"), 404, ReasonJobExpired, false},
		{"404-jobs-off", kit.answer(t, "job-404-jobs-off"), 404, "", false},
		{"405", kit.answer(t, "job-405"), 405, "", false},
		{"failed-embedded-503", kit.answer(t, "job-200-failed-upstream"), 503, ReasonUpstreamUnavailable, false},
		{"unusable-2xx", scriptAnswer{Status: 200, BodyUTF8: `{"status":"running"}`}, 200, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := &waitScript{t: t, answers: []scriptAnswer{tc.answer, succeeded}}
				start := time.Now()
				_, err := s.client(t).Wait(context.Background(), testJobID)
				var de *Error
				if !errors.As(err, &de) {
					t.Fatalf("got %T %v, want *docql.Error", err, err)
				}
				if de.Status != tc.status || de.Reason != tc.reason || de.Retryable != tc.retryable {
					t.Errorf("got %d %q retryable=%v, want %d %q retryable=%v",
						de.Status, de.Reason, de.Retryable, tc.status, tc.reason, tc.retryable)
				}
				if de.JobID != testJobID {
					t.Errorf("JobID = %q, want %q", de.JobID, testJobID)
				}
				if n := len(s.snapshot()); n != 1 {
					t.Errorf("%d requests, want 1", n)
				}
				if elapsed := time.Since(start); elapsed != 0 {
					t.Errorf("returned after %s, want at once", elapsed)
				}
				s.check(t)
			})
		})
	}
}

func TestWaitUnknownStatusPollsUntilTheDeadline(t *testing.T) {
	paused := scriptAnswer{Status: 200, BodyUTF8: fmt.Sprintf(`{"job_id":%q,"status":"paused"}`, testJobID)}
	synctest.Test(t, func(t *testing.T) {
		s := &waitScript{t: t, answers: []scriptAnswer{paused}, repeatLast: true}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := s.client(t).Wait(ctx, testJobID)
		var we *WaitError
		if !errors.As(err, &we) || !we.Timeout() {
			t.Fatalf("got %T %v, want a deadline *WaitError", err, err)
		}
		if got, want := s.gaps(), slices.Repeat([]float64{1}, 9); !slices.Equal(got, want) {
			t.Errorf("sleeps %v, want %v", got, want)
		}
		s.check(t)
	})
}

func TestWaitTwiceOnATerminalJob(t *testing.T) {
	kit := loadWaitKit(t)
	_, succeeded := runningAndSucceeded(t, kit)
	cancelled := kit.answer(t, "job-200-cancelled")
	synctest.Test(t, func(t *testing.T) {
		s := &waitScript{t: t, answers: []scriptAnswer{succeeded, succeeded, cancelled}}
		c := s.client(t)
		first, err1 := c.Wait(context.Background(), testJobID)
		second, err2 := c.Wait(context.Background(), testJobID)
		third, err3 := c.Wait(context.Background(), testJobID)
		if err1 != nil || err2 != nil || err3 != nil {
			t.Fatalf("errors %v %v %v", err1, err2, err3)
		}
		if first.Status != JobStatusSucceeded || second.Status != JobStatusSucceeded || first.Result == nil {
			t.Errorf("got %q %q, want succeeded twice with a result", first.Status, second.Status)
		}
		if third.Status != JobStatusCancelled {
			t.Errorf("got %q, want cancelled", third.Status)
		}
		if gaps := s.gaps(); slices.ContainsFunc(gaps, func(g float64) bool { return g != 0 }) {
			t.Errorf("a terminal job made Wait sleep: %v", gaps)
		}
		s.check(t)
	})
}
