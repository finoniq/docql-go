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

// waitKitCases selects the stub-target via:wait cases without a cancel step
// (CancelJob is not built yet). The selection happens before any subtest
// starts, so the contract workflow never sees a SKIP line.
func waitKitCases(kit *jobKitFile) []*jobKitCase {
	var out []*jobKitCase
	for i := range kit.Cases {
		c := &kit.Cases[i]
		if c.Target != "stub" || c.Via != "wait" {
			continue
		}
		if slices.ContainsFunc(c.Steps, func(s jobKitStep) bool { return s.Op == "cancel" }) {
			continue
		}
		out = append(out, c)
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
	if want := []string{"wait-succeeded", "wait-failed"}; !slices.Equal(ids, want) {
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
