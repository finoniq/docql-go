package docql

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The kit's via:wait job cases through the public job methods, against the
// stub (JSDK-01). Step semantics follow the kit's reference runners
// (docql-console deploy/scripts/job-cases-edge.mjs and
// apps/docql-stub/test/job-kit.ts runJobCase): a per-run {run} token keeps
// each case's fingerprint unique, submit posts the document through
// SubmitDocument and binds the answered job id, cancel calls CancelJob, and
// wait calls Wait and reads the outcome the way the kit's waitForJob reports
// it.

// jobKitStep is one step of a job case, as far as the wait cases use it.
type jobKitStep struct {
	Op      string      `json:"op"`
	As      string      `json:"as"`
	Job     string      `json:"job"`
	Key     string      `json:"key"`
	Request caseRequest `json:"request"`
	Expect  struct {
		Status  int     `json:"status"`
		Outcome string  `json:"outcome"`
		Reason  *string `json:"reason"`
	} `json:"expect"`
}

type jobKitCase struct {
	ID     string       `json:"id"`
	Target string       `json:"target"`
	Via    string       `json:"via"`
	Steps  []jobKitStep `json:"steps"`
}

type jobKitFile struct {
	Keys struct {
		Valid string `json:"valid"`
	} `json:"keys"`
	Cases []jobKitCase `json:"cases"`
}

func loadJobKit(t *testing.T) *jobKitFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("contract", "job-cases.json"))
	if err != nil {
		t.Fatalf("read contract/job-cases.json: %v", err)
	}
	var kit jobKitFile
	if err := json.Unmarshal(raw, &kit); err != nil {
		t.Fatalf("decode contract/job-cases.json: %v", err)
	}
	return &kit
}

// runToken is the per-run {run} token: 8 base62 characters (kit invariant 8).
func runToken(t *testing.T) string {
	t.Helper()
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	var b strings.Builder
	for range 8 {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			t.Fatalf("run token: %v", err)
		}
		b.WriteByte(alphabet[n.Int64()])
	}
	token := b.String()
	if !regexp.MustCompile(`^[A-Za-z0-9]{8}$`).MatchString(token) {
		t.Fatalf("run token %q is not 8 base62 characters", token)
	}
	return token
}

// waitKitCases selects the stub-target via:wait cases. The selection happens
// before any subtest starts, so the contract workflow never sees a SKIP line.
func waitKitCases(kit *jobKitFile) []*jobKitCase {
	var out []*jobKitCase
	for i := range kit.Cases {
		c := &kit.Cases[i]
		if c.Target == "stub" && c.Via == "wait" {
			out = append(out, c)
		}
	}
	return out
}

// seenRequest is one exchange the recording transport saw: the method, the
// status and, for a 2xx job answer, the job status in its body.
type seenRequest struct {
	method    string
	status    int
	jobStatus string
}

// recordingTransport records every exchange of the SDK client and passes it
// to the default transport unchanged.
type recordingTransport struct {
	mu   sync.Mutex
	seen []seenRequest
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	rec := seenRequest{method: req.Method, status: resp.StatusCode}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		body, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr != nil {
			return nil, rerr
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		var doc struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(body, &doc)
		rec.jobStatus = doc.Status
	}
	r.mu.Lock()
	r.seen = append(r.seen, rec)
	r.mu.Unlock()
	return resp, nil
}

func (r *recordingTransport) snapshot() []seenRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.seen)
}

// TestContractJobWaitCases runs the kit's via:wait cases through
// SubmitDocument, CancelJob and Wait against the stub.
func TestContractJobWaitCases(t *testing.T) {
	skipWithoutStub(t)
	kit := loadJobKit(t)
	cases := waitKitCases(kit)
	ids := make([]string, 0, len(cases))
	for _, c := range cases {
		ids = append(ids, c.ID)
	}
	if want := []string{"wait-succeeded", "wait-failed", "wait-cancelled"}; !slices.Equal(ids, want) {
		t.Fatalf("selected wait cases %v, want %v", ids, want)
	}
	run := runToken(t)
	ran := 0
	for _, c := range cases {
		t.Run(c.ID, func(t *testing.T) {
			runWaitKitCase(t, kit, c, run)
			ran++
		})
	}
	if ran != len(cases) {
		t.Fatalf("ran %d of %d wait cases: a subtest stopped before its last assertion", ran, len(cases))
	}
}

func runWaitKitCase(t *testing.T, kit *jobKitFile, c *jobKitCase, run string) {
	t.Helper()
	base := stubURL(t)
	rec := &recordingTransport{}
	client, err := NewClient(WithAPIKey(kit.Keys.Valid), WithAPIURL(base),
		WithHTTPClient(&http.Client{Transport: rec}))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx := context.Background()
	sub := func(s string) string { return strings.ReplaceAll(s, "{run}", run) }
	jobs := map[string]string{}

	for i, step := range c.Steps {
		switch step.Op {
		case "submit":
			if step.Key != "" && step.Key != "valid" {
				t.Fatalf("step %d: a wait case submits with the valid key, not %q", i, step.Key)
			}
			if step.Expect.Status != 202 {
				t.Fatalf("step %d: a wait case submit expects 202, not %d", i, step.Expect.Status)
			}
			req := step.Request
			var instr Instruction
			switch {
			case req.Query != nil:
				instr = Query(sub(*req.Query))
			case req.Prompt != nil:
				instr = Prompt(sub(*req.Prompt))
			default:
				t.Fatalf("step %d: submit carries neither query nor prompt", i)
			}
			opts := []QueryOption{WithFilename(req.File.Name)}
			if req.Mode != nil {
				opts = append(opts, WithMode(Mode(*req.Mode)))
			}
			job, err := client.SubmitDocument(ctx, FileBytes(caseFileBytes(t, req.File), req.File.Name), instr, opts...)
			if err != nil {
				t.Fatalf("step %d: SubmitDocument: %v", i, err)
			}
			if job.Status != JobStatusQueued {
				t.Fatalf("step %d: submitted job status %q, want queued", i, job.Status)
			}
			if job.JobID == "" || job.IdempotencyKey == "" {
				t.Fatalf("step %d: submitted job without a job id or an idempotency key", i)
			}
			jobs[step.As] = job.JobID

		case "cancel":
			jobID := boundJob(t, jobs, step, i)
			job, err := client.CancelJob(ctx, jobID)
			if err != nil {
				t.Fatalf("step %d: CancelJob: %v", i, err)
			}
			if step.Expect.Status != 202 {
				t.Fatalf("step %d: a wait case cancel expects 202, not %d", i, step.Expect.Status)
			}
			if job.JobID != jobID || job.Status != JobStatusCancelled {
				t.Fatalf("step %d: cancel answered %q %q, want the cancelled job", i, job.JobID, job.Status)
			}

		case "wait":
			jobID := boundJob(t, jobs, step, i)
			before := len(rec.snapshot())
			outcome, reason := waitOutcome(t, client, rec, jobID)
			for _, r := range rec.snapshot()[before:] {
				if r.method != http.MethodGet {
					t.Fatalf("step %d: Wait sent %s; it only ever sends GET", i, r.method)
				}
			}
			if step.Expect.Outcome != "" && outcome != step.Expect.Outcome {
				t.Fatalf("step %d: outcome %s, want %s", i, outcome, step.Expect.Outcome)
			}
			if step.Expect.Reason != nil && reason != *step.Expect.Reason {
				t.Fatalf("step %d: reason %q, want %q", i, reason, *step.Expect.Reason)
			}

		default:
			t.Fatalf("step %d: unsupported op %q in a wait case", i, step.Op)
		}
	}
}

func boundJob(t *testing.T, jobs map[string]string, step jobKitStep, i int) string {
	t.Helper()
	jobID, ok := jobs[step.Job]
	if !ok {
		t.Fatalf("step %d: job %q was never bound by a submit", i, step.Job)
	}
	return jobID
}

// waitOutcome calls Wait and names its result the way the kit's waitForJob
// does: succeeded or cancelled (a Job), failed (the job's error, not
// retryable), error (any other *Error) or timeout (*WaitError).
func waitOutcome(t *testing.T, client *Client, rec *recordingTransport, jobID string) (outcome, reason string) {
	t.Helper()
	job, err := client.Wait(context.Background(), jobID)
	if err == nil {
		if job.JobID != jobID {
			t.Fatalf("Wait returned job %q, want %q", job.JobID, jobID)
		}
		switch job.Status {
		case JobStatusSucceeded:
			if job.Result == nil {
				t.Fatal("a succeeded job without a result")
			}
		case JobStatusCancelled:
			if job.Result != nil {
				t.Fatal("a cancelled job with a result")
			}
		default:
			t.Fatalf("Wait returned non-terminal status %q", job.Status)
		}
		return string(job.Status), ""
	}
	var we *WaitError
	if errors.As(err, &we) {
		if we.JobID != jobID {
			t.Fatalf("WaitError.JobID = %q, want %q", we.JobID, jobID)
		}
		return "timeout", ""
	}
	var de *Error
	if !errors.As(err, &de) {
		t.Fatalf("Wait: expected *docql.Error or *docql.WaitError, got %T: %v", err, err)
	}
	if de.JobID != jobID {
		t.Fatalf("Error.JobID = %q, want %q", de.JobID, jobID)
	}
	// A failed job's error is built from the embedded envelope of the last
	// answer, a 200 with status failed: never retryable.
	seen := rec.snapshot()
	if last := seen[len(seen)-1]; last.status == http.StatusOK && last.jobStatus == string(JobStatusFailed) {
		if de.Retryable {
			t.Fatal("a failed job's error is retryable")
		}
		return "failed", de.Reason
	}
	return "error", de.Reason
}

// countedRequest is one exchange the counting transport saw.
type countedRequest struct {
	keys       []string
	status     int
	replayed   bool
	retryAfter string
}

// countingTransport passes every request to the default transport and
// records the Idempotency-Key lines it carried and what came back, so the
// submit tests count the SDK-level requests that reached the stub.
type countingTransport struct {
	mu   sync.Mutex
	seen []countedRequest
}

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.seen = append(c.seen, countedRequest{
		keys:       req.Header.Values("Idempotency-Key"),
		status:     resp.StatusCode,
		replayed:   resp.Header.Get("Idempotent-Replayed") == "true",
		retryAfter: resp.Header.Get("Retry-After"),
	})
	c.mu.Unlock()
	return resp, nil
}

func (c *countingTransport) snapshot() []countedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.seen)
}

func (c *countingTransport) statuses() []int {
	var out []int
	for _, r := range c.snapshot() {
		out = append(out, r.status)
	}
	return out
}

// TestContractJobSubmit runs the public, retrying SubmitDocument against the
// stub's persist and hold knobs (JSDK-02, D-07), unlike TestContractJobCases,
// whose wire steps use the single-attempt sendOnce (26-RESEARCH F8). The knobs
// are filename-based and live because the kit's stubEnv sets
// STUB_TEST_KNOBS=true: persist-unknown creates the job and still answers 503
// Retry-After 5 (a same-key retry replays the 202), persist-503 answers 503
// Retry-After 5 without a job, and hold-1500 keeps the key reserved for
// 1.5 s, so a concurrent same-key submit gets 409 idempotency_in_progress
// Retry-After 1. The retry sleeps are real. Every query and key embeds a
// per-run token, so a rerun against a long-lived stub never replays an
// earlier run's job.
func TestContractJobSubmit(t *testing.T) {
	skipWithoutStub(t)
	kit := loadJobKit(t)
	var pdf []byte
	for _, c := range kit.Cases {
		if c.ID == "j28-unknown" {
			pdf = caseFileBytes(t, c.Steps[0].Request.File)
		}
	}
	if len(pdf) == 0 {
		t.Fatal("kit case j28-unknown carries no document")
	}
	run := runToken(t)
	query := func(name string) Instruction {
		return Query("What is the total amount? (p26-09-" + name + "-" + run + ")")
	}
	key := func(name string) string { return "p26-09-" + name + "-" + run }
	newClient := func(t *testing.T) (*Client, *countingTransport) {
		t.Helper()
		ct := &countingTransport{}
		c, err := NewClient(WithAPIKey(kit.Keys.Valid), WithAPIURL(stubURL(t)), WithHTTPClient(&http.Client{Transport: ct}))
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		return c, ct
	}
	ctx := context.Background()

	t.Run("persist_unknown", func(t *testing.T) {
		c, ct := newClient(t)
		defer func() { t.Logf("SDK-level requests: %v", ct.statuses()) }()
		k := key("persist-unknown")
		job, err := c.SubmitDocument(ctx, FileBytes(pdf, "persist-unknown.pdf"), query("persist-unknown"), WithIdempotencyKey(k))
		if err != nil {
			t.Fatalf("SubmitDocument: %v", err)
		}
		if job.Status != JobStatusQueued || job.IdempotencyKey != k || job.JobID == "" {
			t.Fatalf("got %q %q key %q, want the queued job with the key", job.JobID, job.Status, job.IdempotencyKey)
		}
		seen := ct.snapshot()
		if got := ct.statuses(); !slices.Equal(got, []int{503, 202}) {
			t.Fatalf("statuses %v, want [503 202]", got)
		}
		for i, r := range seen {
			if !slices.Equal(r.keys, []string{k}) {
				t.Errorf("request %d keys %q, want [%q]", i+1, r.keys, k)
			}
		}
		if seen[0].retryAfter != "5" || !seen[1].replayed {
			t.Errorf("the 503 Retry-After %q and replayed %v, want 5 and a replayed 202", seen[0].retryAfter, seen[1].replayed)
		}
		// The replayed job is the one DocQL created on the first (503)
		// attempt, so it exists. The kit's stubEnv keeps a job for 5 s and
		// the retry slept 5 s, so the owner may already see job_expired;
		// job_not_found would mean the replay pointed at a job that never
		// existed.
		got, err := c.GetJob(ctx, job.JobID)
		switch {
		case err == nil:
			if got.JobID != job.JobID {
				t.Errorf("GetJob answered job %q, want %q", got.JobID, job.JobID)
			}
		default:
			var de *Error
			if !errors.As(err, &de) || de.Reason != ReasonJobExpired {
				t.Errorf("GetJob on the replayed job: %v, want the job or job_expired", err)
			}
		}
	})

	t.Run("persist_503", func(t *testing.T) {
		c, ct := newClient(t)
		defer func() { t.Logf("SDK-level requests: %v", ct.statuses()) }()
		k := key("persist-503")
		_, err := c.SubmitDocument(ctx, FileBytes(pdf, "persist-503.pdf"), query("persist-503"), WithIdempotencyKey(k))
		var de *Error
		if !errors.As(err, &de) {
			t.Fatalf("got %T %v, want *docql.Error", err, err)
		}
		if de.Status != 503 || de.Reason != ReasonUpstreamUnavailable || de.IdempotencyKey != k || de.RetryAfter != 5*time.Second {
			t.Fatalf("got %d %q key %q Retry-After %s, want 503 upstream_unavailable with the key and 5s", de.Status, de.Reason, de.IdempotencyKey, de.RetryAfter)
		}
		if got := ct.statuses(); !slices.Equal(got, []int{503, 503, 503, 503}) {
			t.Fatalf("statuses %v, want four 503s", got)
		}
		for i, r := range ct.snapshot() {
			if !slices.Equal(r.keys, []string{k}) {
				t.Errorf("request %d keys %q, want [%q]", i+1, r.keys, k)
			}
		}
	})

	t.Run("same_key_twice", func(t *testing.T) {
		c, ct := newClient(t)
		defer func() { t.Logf("SDK-level requests: %v", ct.statuses()) }()
		k := key("same-key-twice")
		first, err1 := c.SubmitDocument(ctx, FileBytes(pdf, "twice.pdf"), query("same-key-twice"), WithIdempotencyKey(k))
		second, err2 := c.SubmitDocument(ctx, FileBytes(pdf, "twice.pdf"), query("same-key-twice"), WithIdempotencyKey(k))
		if err1 != nil || err2 != nil {
			t.Fatalf("SubmitDocument: %v, %v", err1, err2)
		}
		if first.JobID != second.JobID {
			t.Errorf("job ids %q and %q, want the same job", first.JobID, second.JobID)
		}
		seen := ct.snapshot()
		if got := ct.statuses(); !slices.Equal(got, []int{202, 202}) {
			t.Fatalf("statuses %v, want [202 202]", got)
		}
		if seen[0].replayed || !seen[1].replayed {
			t.Errorf("replayed %v %v, want false then true", seen[0].replayed, seen[1].replayed)
		}
	})

	t.Run("hold_concurrent", func(t *testing.T) {
		c, ct := newClient(t)
		defer func() { t.Logf("SDK-level requests: %v", ct.statuses()) }()
		k := key("hold-concurrent")
		var wg sync.WaitGroup
		jobs := make([]*Job, 2)
		errs := make([]error, 2)
		// The kit's j16 starts the second submit 300 ms after the first.
		for i, delay := range []time.Duration{0, 300 * time.Millisecond} {
			wg.Go(func() {
				time.Sleep(delay)
				jobs[i], errs[i] = c.SubmitDocument(ctx, FileBytes(pdf, "hold-1500.pdf"), query("hold-concurrent"), WithIdempotencyKey(k))
			})
		}
		wg.Wait()
		if errs[0] != nil || errs[1] != nil {
			t.Fatalf("SubmitDocument: %v, %v", errs[0], errs[1])
		}
		if jobs[0].JobID != jobs[1].JobID || jobs[0].IdempotencyKey != k || jobs[1].IdempotencyKey != k {
			t.Fatalf("jobs %q/%q keys %q/%q, want one job with the key", jobs[0].JobID, jobs[1].JobID, jobs[0].IdempotencyKey, jobs[1].IdempotencyKey)
		}
		seen := ct.snapshot()
		var conflicts int
		var replayed []bool
		for i, r := range seen {
			if !slices.Equal(r.keys, []string{k}) {
				t.Errorf("request %d keys %q, want [%q]", i+1, r.keys, k)
			}
			switch r.status {
			case 409:
				conflicts++
				if r.retryAfter != "1" {
					t.Errorf("a 409 with Retry-After %q, want 1", r.retryAfter)
				}
			case 202:
				replayed = append(replayed, r.replayed)
			default:
				t.Errorf("request %d answered %d", i+1, r.status)
			}
		}
		if conflicts == 0 {
			t.Error("no 409 idempotency_in_progress: the second submit did not overlap the hold")
		}
		// One fresh 202 (the first submit) and one replay (the second,
		// after its 409s).
		slices.SortFunc(replayed, func(a, b bool) int {
			if a == b {
				return 0
			}
			if !a {
				return -1
			}
			return 1
		})
		if !slices.Equal(replayed, []bool{false, true}) {
			t.Errorf("202 replayed flags %v, want one fresh and one replayed", replayed)
		}
	})

	t.Run("non_replayable", func(t *testing.T) {
		c, ct := newClient(t)
		defer func() { t.Logf("SDK-level requests: %v", ct.statuses()) }()
		k := key("non-replayable")
		pr, pw := io.Pipe()
		go func() {
			_, _ = pw.Write(pdf)
			_ = pw.Close()
		}()
		_, err := c.SubmitDocument(ctx, FileReader(pr, "persist-unknown.pdf"), query("non-replayable"), WithIdempotencyKey(k))
		var de *Error
		if !errors.As(err, &de) || de.Status != 503 || de.IdempotencyKey != k {
			t.Fatalf("got %T %v, want the 503 *docql.Error with the key", err, err)
		}
		if got := ct.statuses(); !slices.Equal(got, []int{503}) {
			t.Fatalf("statuses %v, want one 503", got)
		}
	})
}
