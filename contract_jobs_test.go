package docql

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Every stub-target kit v3 job case through the SDK's own wire layer
// (JSDK-04). This is a port of the kit's reference runners (docql-console
// deploy/scripts/job-cases-edge.mjs runCase and apps/docql-stub/test/job-kit.ts
// runJobCase), op by op, as docql-python's tests/contract/test_job_cases.py
// ports them (26-06), not a new interpretation. Two layers (26-RESEARCH
// Pattern 5):
//
//   - submit, join, poll and cancel steps call the unexported single-attempt
//     sendOnce exactly once per request, never the retrying SubmitDocument
//     (26-RESEARCH F8: j16, j28 and j28-unknown assert the raw first answer).
//     Each answer is checked raw (every wire field the kit names) and through
//     the SDK's decoders (parseJob for a 2xx, decodeError otherwise).
//   - wait steps call the public Wait; sync steps call the public
//     QueryDocument.
//
// Step semantics, as in the reference runners: {run} is replaced in query,
// prompt and idempotency-key strings with one 8-character base62 token per
// test run; as binds the job id of a 2xx submit and job names it; key picks
// the kit's valid, validOther or invalid API key; an async submit runs on its
// own goroutine until its join; delayBeforeMs and sleep use real time (the
// kit's stubEnv keeps a job for 5 s); a step's boundary (else the kit's
// default) goes through v0.1's boundary seam newBoundary; an idempotencyKey is
// one string, a lines list sent as several header lines through sendOnce's
// keys slice, or a repeat x length string. Every answer from a job route,
// including each GET inside Wait, must meet the kit's job-route invariants.
//
// Cases are independent: each one builds its own clients, transport,
// bindings and captures, and every fingerprint and key embeds the case id
// plus the run token, so the order the cases run in (-shuffle=on) never
// changes an outcome.

// jobKitStep is one step of a kit job case.
type jobKitStep struct {
	Op             string                     `json:"op"`
	As             string                     `json:"as"`
	Job            string                     `json:"job"`
	ID             *string                    `json:"id"`
	Key            *string                    `json:"key"`
	Name           string                     `json:"name"`
	Async          bool                       `json:"async"`
	Boundary       *string                    `json:"boundary"`
	DelayBeforeMs  *int                       `json:"delayBeforeMs"`
	Seconds        *float64                   `json:"seconds"`
	Until          *string                    `json:"until"`
	TimeoutSeconds *float64                   `json:"timeoutSeconds"`
	IdempotencyKey json.RawMessage            `json:"idempotencyKey"`
	Request        *caseRequest               `json:"request"`
	Expect         map[string]json.RawMessage `json:"expect"`
}

type jobKitCase struct {
	ID     string       `json:"id"`
	Target string       `json:"target"`
	Via    string       `json:"via"`
	Ref    *string      `json:"ref"`
	Steps  []jobKitStep `json:"steps"`
}

type jobKitFile struct {
	Keys struct {
		Valid      string `json:"valid"`
		ValidOther string `json:"validOther"`
		Invalid    string `json:"invalid"`
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

// The vias the kit knows, and the ones this runner replays.
var (
	knownJobVias = []string{"submit", "poll", "cancel", "wait", "sync", "infra"}
	runJobVias   = []string{"submit", "poll", "cancel", "wait"}
)

// selectedJobCases returns every stub-target case with a replayed via (44 in
// kit v3). The selection happens before any subtest starts, so the contract
// workflow never sees a SKIP line.
func selectedJobCases(kit *jobKitFile) []*jobKitCase {
	var out []*jobKitCase
	for i := range kit.Cases {
		c := &kit.Cases[i]
		if c.Target == "stub" && slices.Contains(runJobVias, c.Via) {
			out = append(out, c)
		}
	}
	return out
}

// jobCaseExclusions excludes every other case by exactly one named rule (the
// first that matches).
var jobCaseExclusions = []struct {
	name string
	rule func(*jobKitCase) bool
}{
	{"target real: runs only against the real DocQL (kit invariant 7)", func(c *jobKitCase) bool { return c.Target == "real" }},
	{"target console: needs the trusted key and tenant header", func(c *jobKitCase) bool { return c.Target == "console" }},
	{"via sync: sdk-cases.json, run by TestContractSDKCases/TestContractRawCases", func(c *jobKitCase) bool { return c.Via == "sync" }},
	{"via infra: deployment checks, no SDK call", func(c *jobKitCase) bool { return c.Via == "infra" }},
}

// TestContractJobCaseClassification: each kit case is run by
// TestContractJobCases or excluded by one named rule, and every via is
// known; a new kind of case fails loudly instead of silently disappearing.
func TestContractJobCaseClassification(t *testing.T) {
	kit := loadJobKit(t)
	selected := selectedJobCases(kit)
	if len(selected) == 0 {
		t.Fatal("no stub job case selected")
	}
	excluded := map[string][]string{}
	for i := range kit.Cases {
		c := &kit.Cases[i]
		if !slices.Contains(knownJobVias, c.Via) {
			t.Errorf("case %s has an unknown via %q", c.ID, c.Via)
		}
		if slices.Contains(selected, c) {
			if c.Ref != nil || len(c.Steps) == 0 {
				t.Errorf("selected case %s has a ref or no steps", c.ID)
			}
			continue
		}
		matched := false
		for _, ex := range jobCaseExclusions {
			if ex.rule(c) {
				excluded[ex.name] = append(excluded[ex.name], c.ID)
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("case %s is neither run nor excluded by a named rule", c.ID)
		}
	}
	t.Logf("run %d, excluded %v", len(selected), excluded)
}

// jobDefaultBoundary is job-kit.ts DEFAULT_BOUNDARY: the multipart boundary
// of a step that names none.
const jobDefaultBoundary = "docqlsdktestboundary0000000001"

// pollUntilSeconds: a poll-until gives up after this many seconds of real
// sleeping (job-cases-edge.mjs).
const pollUntilSeconds = 120

// implementedJobExpect is every expect field job-kit.ts checks.
var implementedJobExpect = []string{
	"status", "reason", "retryAfterSeconds", "location", "replayed", "jobStatus",
	"scope", "header", "pageCount", "resultPageCount", "embeddedReason",
	"embeddedStatus", "sameJobAs", "differentJobFrom", "bodyEquals",
	"bodyEqualsExceptRequestId", "requestIdDiffersFrom", "resultEqualsSync",
	"errorEqualsSync", "states",
}

var (
	jobKeysOrder      = []string{"job_id", "status", "metadata", "created_at", "started_at", "finished_at", "expires_at"}
	jobMetadataOrder  = []string{"page_count", "pages_done", "request_id"}
	secondTimestampRE = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$`)
	wholeSecondsRE    = regexp.MustCompile(`^[1-9][0-9]*$`)
)

// wireAnswer is one answer as it came off the wire.
type wireAnswer struct {
	status int
	header http.Header
	body   []byte
}

// capturedExchange is one request and its answer, as the capturing transport
// saw them.
type capturedExchange struct {
	method string
	path   string
	wire   wireAnswer
}

// capturingTransport is the default transport plus a record of every answer,
// its body read in full and handed on unchanged.
type capturingTransport struct {
	mu   sync.Mutex
	seen []capturedExchange
}

func (c *capturingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	c.mu.Lock()
	c.seen = append(c.seen, capturedExchange{method: req.Method, path: req.URL.Path,
		wire: wireAnswer{status: resp.StatusCode, header: resp.Header.Clone(), body: body}})
	c.mu.Unlock()
	return resp, nil
}

func (c *capturingTransport) snapshot() []capturedExchange {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.seen)
}

// caseError fails a case with its step location.
type caseError struct{ msg string }

func (e *caseError) Error() string { return e.msg }

func failCase(format string, args ...any) { panic(&caseError{fmt.Sprintf(format, args...)}) }

// jsonValue decodes JSON bytes into an any (nil when not JSON).
func jsonValue(b []byte) any {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil
	}
	return v
}

// fieldOf walks a decoded JSON object along path; nil when a step is missing.
func fieldOf(v any, path ...string) any {
	for _, k := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

func stringOrNil(v any) any {
	if s, ok := v.(string); ok {
		return s
	}
	return nil
}

// sameJSON is deep JSON equality on decoded values: like node's
// isDeepStrictEqual on parsed JSON, it never equates true with 1.
func sameJSON(a, b any) bool { return reflect.DeepEqual(normJSON(a), normJSON(b)) }

// normJSON round-trips a Go value through JSON so typed values (int, *int,
// string) compare with decoded ones (float64, nil, string).
func normJSON(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("unmarshalable %T", v)
	}
	return jsonValue(b)
}

// objectKeys returns the keys of a JSON object in wire order.
func objectKeys(raw []byte) ([]string, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, false
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, false
		}
	}
	return keys, true
}

// withoutRequestID returns a deep copy of v without metadata.request_id (and,
// when asked, metadata.extraction_status).
func withoutRequestID(v any, dropExtractionStatus bool) any {
	cp := normJSON(v)
	if md, ok := fieldOf(cp, "metadata").(map[string]any); ok {
		delete(md, "request_id")
		if dropExtractionStatus {
			delete(md, "extraction_status")
		}
	}
	return cp
}

// checkJobInvariants applies kit invariants 1-5 to one job-route answer
// (job-kit.ts checkJobInvariants).
func checkJobInvariants(where string, w wireAnswer) {
	fail := func(format string, args ...any) { failCase("%s: invariant: %s", where, fmt.Sprintf(format, args...)) }
	if cc := w.header.Get("Cache-Control"); cc != "no-store" {
		fail("Cache-Control is %q, not no-store", cc)
	}
	rid := w.header.Get("X-Request-Id")
	if rid == "" {
		fail("no X-Request-Id")
	}
	body := jsonValue(w.body)
	obj, _ := body.(map[string]any)
	if _, ok := obj["error_info"]; ok && fieldOf(body, "metadata", "request_id") != rid {
		fail("envelope metadata.request_id differs from X-Request-Id")
	}
	if _, ok := obj["job_id"]; ok {
		want := slices.Clone(jobKeysOrder)
		switch obj["status"] {
		case "succeeded":
			want = append(want, "result")
		case "failed":
			want = append(want, "error")
		}
		if keys, _ := objectKeys(w.body); !slices.Equal(keys, want) {
			fail("job keys %s", strings.Join(keys, ","))
		}
		var top map[string]json.RawMessage
		_ = json.Unmarshal(w.body, &top)
		if keys, ok := objectKeys(top["metadata"]); !ok || !slices.Equal(keys, jobMetadataOrder) {
			fail("job metadata keys out of order")
		}
		for _, k := range []string{"created_at", "started_at", "finished_at", "expires_at"} {
			v := obj[k]
			if v == nil {
				continue
			}
			if s, ok := v.(string); !ok || !secondTimestampRE.MatchString(s) {
				fail("%s %v is not second-precision RFC 3339 UTC", k, v)
			}
		}
		if _, ok := obj["created_at"].(string); !ok {
			fail("created_at is null")
		}
	}
	if ra := w.header.Values("Retry-After"); len(ra) > 0 && !wholeSecondsRE.MatchString(ra[0]) {
		fail("Retry-After %s is not a whole number >= 1", ra[0])
	}
	if loc := w.header.Values("Location"); len(loc) > 0 {
		id, ok := fieldOf(body, "job_id").(string)
		path, _ := jobPath(id)
		if !ok || loc[0] != path {
			fail("Location %s is not /v1/jobs/{job_id}", loc[0])
		}
	}
}

func dedupeStates(states []string) []string {
	var out []string
	for i, s := range states {
		if i == 0 || states[i-1] != s {
			out = append(out, s)
		}
	}
	return out
}

func isSubsequence(observed, allowed []string) bool {
	j := 0
	for _, s := range observed {
		for j < len(allowed) && allowed[j] != s {
			j++
		}
		if j == len(allowed) {
			return false
		}
		j++
	}
	return true
}

// pendingSubmit is an async submit running until its join.
type pendingSubmit struct {
	done chan struct{}
	name string
	wire wireAnswer
	err  any
}

// jobCaseRunner runs one kit case.
type jobCaseRunner struct {
	t         *testing.T
	kit       *jobKitFile
	c         *jobKitCase
	run       string
	transport *capturingTransport
	clients   map[string]*Client
	keys      map[string]string
	jobs      map[string]string
	pending   map[string]*pendingSubmit
	captured  map[string]wireAnswer
	checked   int
}

// boundarySeam serializes every use of the newBoundary seam: an async
// submit frames its upload on its own goroutine.
var boundarySeam sync.Mutex

func newJobCaseRunner(t *testing.T, kit *jobKitFile, c *jobKitCase, run, base string) *jobCaseRunner {
	t.Helper()
	r := &jobCaseRunner{
		t: t, kit: kit, c: c, run: run,
		transport: &capturingTransport{},
		clients:   map[string]*Client{},
		keys:      map[string]string{"valid": kit.Keys.Valid, "validOther": kit.Keys.ValidOther, "invalid": kit.Keys.Invalid},
		jobs:      map[string]string{},
		pending:   map[string]*pendingSubmit{},
		captured:  map[string]wireAnswer{},
	}
	for name, key := range r.keys {
		cl, err := NewClient(WithAPIKey(key), WithAPIURL(base), WithHTTPClient(&http.Client{Transport: r.transport}))
		if err != nil {
			t.Fatalf("NewClient(%s): %v", name, err)
		}
		r.clients[name] = cl
	}
	return r
}

func (r *jobCaseRunner) sub(s string) string {
	out := strings.ReplaceAll(s, "{run}", r.run)
	if strings.Contains(out, "{run}") {
		failCase("%s: {run} left in %q", r.c.ID, out)
	}
	return out
}

func (r *jobCaseRunner) keyName(s jobKitStep) string {
	if s.Key == nil {
		return "valid"
	}
	return *s.Key
}

func (r *jobCaseRunner) client(s jobKitStep, where string) *Client {
	cl, ok := r.clients[r.keyName(s)]
	if !ok {
		failCase("%s: key %q is not one the SDK can send", where, r.keyName(s))
	}
	return cl
}

// idempotencyKeys reads a step's idempotencyKey: absent, one string, a lines
// list, or a repeat x length string.
func (r *jobCaseRunner) idempotencyKeys(s jobKitStep, where string) []string {
	raw := s.IdempotencyKey
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{r.sub(one)}
	}
	var shaped struct {
		Lines  []string `json:"lines"`
		Repeat *string  `json:"repeat"`
		Length int      `json:"length"`
	}
	if err := json.Unmarshal(raw, &shaped); err != nil {
		failCase("%s: idempotencyKey %s: %v", where, raw, err)
	}
	if shaped.Lines != nil {
		out := make([]string, 0, len(shaped.Lines))
		for _, l := range shaped.Lines {
			out = append(out, r.sub(l))
		}
		return out
	}
	if shaped.Repeat == nil {
		failCase("%s: idempotencyKey %s has neither lines nor repeat", where, raw)
	}
	return []string{strings.Repeat(*shaped.Repeat, shaped.Length)}
}

func (r *jobCaseRunner) pathOf(s jobKitStep, where string) string {
	id := ""
	if s.ID != nil {
		id = *s.ID
	} else {
		bound, ok := r.jobs[s.Job]
		if !ok {
			failCase("%s: job %s was never bound by a 2xx submit", where, s.Job)
		}
		id = bound
	}
	p, err := jobPath(id)
	if err != nil {
		failCase("%s: job path: %v", where, err)
	}
	return p
}

// instruction and options of a step's request, the {run} token substituted.
func (r *jobCaseRunner) request(s jobKitStep, where string) (File, Instruction, []QueryOption) {
	req := s.Request
	if req == nil {
		failCase("%s: a step with no request", where)
	}
	var instr Instruction
	switch {
	case req.Query != nil:
		instr = Query(r.sub(*req.Query))
	case req.Prompt != nil:
		instr = Prompt(r.sub(*req.Prompt))
	default:
		failCase("%s: request carries neither query nor prompt", where)
	}
	opts := []QueryOption{WithFilename(req.File.Name)}
	if req.Mode != nil {
		opts = append(opts, WithMode(Mode(*req.Mode)))
	}
	return FileBytes(caseFileBytes(r.t, req.File), req.File.Name), instr, opts
}

func (r *jobCaseRunner) boundary(s jobKitStep) string {
	if s.Boundary != nil {
		return *s.Boundary
	}
	return jobDefaultBoundary
}

// sendSubmit sends one submit through sendOnce: the step's keys, its
// boundary, and nothing retried. A key the SDK refuses is first proven
// refused by validateIdempotencyKey (SubmitDocument's guard); sendOnce does
// not validate, so the case can assert the server's own answer to it.
func (r *jobCaseRunner) sendSubmit(s jobKitStep, where string) wireAnswer {
	cl := r.client(s, where)
	keys := r.idempotencyKeys(s, where)
	for _, k := range keys {
		if validateIdempotencyKey(k) != nil {
			if _, err := cl.SubmitDocument(context.Background(), FileBytes([]byte("x"), "x.pdf"), Query("q"), WithIdempotencyKey(k)); !errors.Is(err, ErrInvalidIdempotencyKey) {
				failCase("%s: SubmitDocument accepted the key the kit sends as malformed: %v", where, err)
			}
		}
	}
	file, instr, opts := r.request(s, where)
	var qs querySettings
	for _, o := range opts {
		o(&qs)
	}
	boundarySeam.Lock()
	prev := newBoundary
	b := r.boundary(s)
	newBoundary = func() string { return b }
	up, err := prepareUpload(file, instr, qs)
	newBoundary = prev
	boundarySeam.Unlock()
	if err != nil {
		failCase("%s: prepareUpload: %v", where, err)
	}
	defer up.closer.close()
	status, h, body, err := cl.sendOnce(context.Background(), http.MethodPost, jobsPath, up, keys, cl.cfg.timeout)
	if err != nil {
		failCase("%s: sendOnce: %v", where, err)
	}
	return wireAnswer{status: status, header: h, body: body}
}

func (r *jobCaseRunner) sendJob(s jobKitStep, where, method, path string) wireAnswer {
	cl := r.client(s, where)
	status, h, body, err := cl.sendOnce(context.Background(), method, path, nil, nil, pollTimeout)
	if err != nil {
		failCase("%s: sendOnce %s: %v", where, method, err)
	}
	return wireAnswer{status: status, header: h, body: body}
}

// checkNewAnswers applies the invariants to every job-route answer the
// transport saw since the last check.
func (r *jobCaseRunner) checkNewAnswers(where string) {
	seen := r.transport.snapshot()
	for _, x := range seen[r.checked:] {
		if x.path == jobsPath || strings.HasPrefix(x.path, jobsPath+"/") {
			p := x.path
			if len(p) > 60 {
				p = p[:60]
			}
			checkJobInvariants(fmt.Sprintf("%s %s %s", where, x.method, p), x.wire)
		}
	}
	r.checked = len(seen)
}

func (r *jobCaseRunner) capture(name, where string) wireAnswer {
	w, ok := r.captured[name]
	if !ok {
		failCase("%s: capture %s is missing", where, name)
	}
	return w
}

// expectString decodes an expect value that names a binding or a capture.
func expectString(where, field string, raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		failCase("%s: expect %s is not a string: %s", where, field, raw)
	}
	return s
}

// secondsOrNil is a decoded Retry-After in whole seconds, nil when 0 (the
// SDK's "absent").
func secondsOrNil(d time.Duration) any {
	if d == 0 {
		return nil
	}
	return int(d / time.Second)
}

func (r *jobCaseRunner) checkExpect(where string, expect map[string]json.RawMessage, w wireAnswer, apiKey string, jobRoute bool) {
	for k := range expect {
		if !slices.Contains(implementedJobExpect, k) {
			failCase("%s: expect field not implemented: %s", where, k)
		}
	}
	eq := func(what string, actual any, expected json.RawMessage) {
		if !sameJSON(actual, jsonValue(expected)) {
			failCase("%s: %s: expected %s, got %v", where, what, expected, actual)
		}
	}
	body := jsonValue(w.body)
	ok := w.status >= 200 && w.status < 300
	// The SDK's own decode of the same answer: a Job for a 2xx job answer,
	// else an *Error.
	var job *Job
	var de *Error
	if jobRoute && ok {
		j, err := parseJob(w.status, w.header, w.body, apiKey, "")
		if err != nil {
			failCase("%s: parseJob: %v", where, err)
		}
		job = j
	} else if !ok {
		de = decodeError(w.status, w.header, w.body, apiKey)
	}

	if v, has := expect["status"]; has {
		eq("status", w.status, v)
		if de != nil {
			eq("decoded status", de.Status, v)
		}
	}
	if v, has := expect["reason"]; has {
		eq("reason", stringOrNil(fieldOf(body, "metadata", "reason")), v)
		if de != nil {
			eq("decoded reason", de.Reason, v)
		}
	}
	if v, has := expect["retryAfterSeconds"]; has {
		var raw any
		if vals := w.header.Values("Retry-After"); len(vals) > 0 {
			n, err := strconv.Atoi(vals[0])
			if err != nil {
				failCase("%s: Retry-After %q is not an integer", where, vals[0])
			}
			raw = n
		}
		eq("Retry-After", raw, v)
		var decoded any
		switch {
		case job != nil:
			decoded = secondsOrNil(job.RetryAfter)
		case de != nil:
			decoded = secondsOrNil(de.RetryAfter)
		}
		eq("decoded RetryAfter", decoded, v)
	}
	if v, has := expect["location"]; has {
		var want bool
		_ = json.Unmarshal(v, &want)
		var loc any
		if vals := w.header.Values("Location"); len(vals) > 0 {
			loc = vals[0]
		}
		var wantLoc any
		if want {
			if id, isStr := fieldOf(body, "job_id").(string); isStr {
				wantLoc, _ = jobPath(id)
			}
		}
		if !sameJSON(loc, wantLoc) {
			failCase("%s: Location: expected %v, got %v", where, wantLoc, loc)
		}
	}
	if v, has := expect["replayed"]; has {
		var want bool
		_ = json.Unmarshal(v, &want)
		var got, wantV any
		if vals := w.header.Values("Idempotent-Replayed"); len(vals) > 0 {
			got = vals[0]
		}
		if want {
			wantV = "true"
		}
		if !sameJSON(got, wantV) {
			failCase("%s: Idempotent-Replayed: expected %v, got %v", where, wantV, got)
		}
	}
	if v, has := expect["jobStatus"]; has {
		eq("job status", fieldOf(body, "status"), v)
		var decoded any
		if job != nil {
			decoded = string(job.Status)
		}
		eq("decoded job status", decoded, v)
	}
	if v, has := expect["scope"]; has {
		eq("scope", fieldOf(body, "metadata", "scope"), v)
	}
	if v, has := expect["header"]; has {
		eq("metadata.header", fieldOf(body, "metadata", "header"), v)
	}
	if v, has := expect["pageCount"]; has {
		eq("page_count", fieldOf(body, "metadata", "page_count"), v)
		var decoded *int
		if job != nil {
			decoded = job.PageCount
		}
		eq("decoded PageCount", decoded, v)
	}
	if v, has := expect["resultPageCount"]; has {
		eq("result page_count", fieldOf(body, "result", "metadata", "page_count"), v)
		var decoded *int
		if job != nil && job.Result != nil {
			decoded = job.Result.PageCount
		}
		eq("decoded Result.PageCount", decoded, v)
	}
	if v, has := expect["embeddedReason"]; has {
		eq("embedded reason", fieldOf(body, "error", "metadata", "reason"), v)
		var embedded *Error
		if job != nil {
			embedded = job.Error
		}
		var decoded any
		if embedded != nil {
			decoded = embedded.Reason
		}
		eq("decoded embedded reason", decoded, v)
		if embedded == nil || embedded.Retryable {
			failCase("%s: the embedded error is missing or retryable", where)
		}
	}
	if v, has := expect["embeddedStatus"]; has {
		eq("embedded status", fieldOf(body, "error", "status_code"), v)
		var decoded any
		if job != nil && job.Error != nil {
			decoded = job.Error.Status
		}
		eq("decoded embedded status", decoded, v)
	}
	if v, has := expect["sameJobAs"]; has {
		name := expectString(where, "sameJobAs", v)
		want, bound := r.jobs[name]
		var wantV any
		if bound {
			wantV = want
		}
		if !sameJSON(fieldOf(body, "job_id"), wantV) {
			failCase("%s: job_id: expected job %s (%v), got %v", where, name, wantV, fieldOf(body, "job_id"))
		}
		var decoded any
		if job != nil {
			decoded = job.JobID
		}
		if !sameJSON(decoded, wantV) {
			failCase("%s: decoded JobID: expected %v, got %v", where, wantV, decoded)
		}
	}
	if v, has := expect["differentJobFrom"]; has {
		name := expectString(where, "differentJobFrom", v)
		other, bound := r.jobs[name]
		if !bound || fieldOf(body, "job_id") == other {
			failCase("%s: job_id should differ from job %s", where, name)
		}
	}
	if v, has := expect["bodyEquals"]; has {
		name := expectString(where, "bodyEquals", v)
		if !bytes.Equal(w.body, r.capture(name, where).body) {
			failCase("%s: body differs from %s byte for byte", where, name)
		}
	}
	if v, has := expect["bodyEqualsExceptRequestId"]; has {
		name := expectString(where, "bodyEqualsExceptRequestId", v)
		other := jsonValue(r.capture(name, where).body)
		if !sameJSON(withoutRequestID(body, false), withoutRequestID(other, false)) {
			failCase("%s: body (except request_id) differs from %s", where, name)
		}
	}
	if v, has := expect["requestIdDiffersFrom"]; has {
		name := expectString(where, "requestIdDiffersFrom", v)
		if w.header.Get("X-Request-Id") == r.capture(name, where).header.Get("X-Request-Id") {
			failCase("%s: X-Request-Id equals %s's", where, name)
		}
	}
	// Handoff §7: a job's result (or error) equals the sync body for the
	// same input, except metadata.request_id and, on success,
	// metadata.extraction_status.
	if v, has := expect["resultEqualsSync"]; has {
		name := expectString(where, "resultEqualsSync", v)
		syncBody := jsonValue(r.capture(name, where).body)
		if !sameJSON(withoutRequestID(fieldOf(body, "result"), true), withoutRequestID(syncBody, true)) {
			failCase("%s: result differs from sync %s (§7)", where, name)
		}
	}
	if v, has := expect["errorEqualsSync"]; has {
		name := expectString(where, "errorEqualsSync", v)
		syncBody := jsonValue(r.capture(name, where).body)
		if !sameJSON(withoutRequestID(fieldOf(body, "error"), false), withoutRequestID(syncBody, false)) {
			failCase("%s: error differs from sync %s (§7)", where, name)
		}
	}
}

func (r *jobCaseRunner) finish(s jobKitStep, where string, w wireAnswer, jobRoute bool, bind string) {
	if id, ok := fieldOf(jsonValue(w.body), "job_id").(string); ok && bind != "" && w.status < 300 {
		r.jobs[bind] = id
	}
	if s.Expect != nil {
		expect := map[string]json.RawMessage{}
		for k, v := range s.Expect {
			if k != "states" {
				expect[k] = v
			}
		}
		r.checkExpect(where, expect, w, r.keys[r.keyName(s)], jobRoute)
	}
}

// runCase runs every step; a case failure is a caseError panic, recovered
// here into t.Fatal.
func (r *jobCaseRunner) runCase() {
	defer func() {
		if p := recover(); p != nil {
			if ce, ok := p.(*caseError); ok {
				r.t.Fatal(ce.msg)
			}
			panic(p)
		}
	}()
	for i, s := range r.c.Steps {
		where := fmt.Sprintf("%s step %d (%s)", r.c.ID, i, s.Op)
		if s.DelayBeforeMs != nil {
			time.Sleep(time.Duration(*s.DelayBeforeMs) * time.Millisecond)
		}
		var w *wireAnswer
		switch s.Op {
		case "sleep":
			if s.Seconds == nil {
				failCase("%s: sleep without seconds", where)
			}
			time.Sleep(time.Duration(*s.Seconds * float64(time.Second)))
		case "submit":
			if s.Async {
				r.startAsync(s, where)
			} else {
				a := r.sendSubmit(s, where)
				r.finish(s, where, a, true, s.As)
				w = &a
			}
		case "join":
			a := r.join(s, where)
			w = &a
		case "sync":
			a := r.syncStep(s, where)
			w = &a
		case "cancel":
			a := r.sendJob(s, where, http.MethodDelete, r.pathOf(s, where))
			r.finish(s, where, a, true, "")
			w = &a
		case "poll":
			a := r.poll(s, where)
			w = &a
		case "wait":
			r.wait(s, where)
		default:
			failCase("%s: unsupported op %q", where, s.Op)
		}
		r.checkNewAnswers(where)
		if w != nil && s.Name != "" && !(s.Op == "submit" && s.Async) {
			r.captured[s.Name] = *w
		}
	}
	if len(r.pending) > 0 {
		var names []string
		for name, p := range r.pending {
			<-p.done
			names = append(names, name)
		}
		failCase("%s: async submit never joined: %v", r.c.ID, names)
	}
}

func (r *jobCaseRunner) startAsync(s jobKitStep, where string) {
	if s.As == "" {
		failCase("%s: async without as", where)
	}
	p := &pendingSubmit{done: make(chan struct{}), name: s.Name}
	r.pending[s.As] = p
	go func() {
		defer close(p.done)
		defer func() { p.err = recover() }()
		p.wire = r.sendSubmit(s, where)
	}()
}

func (r *jobCaseRunner) join(s jobKitStep, where string) wireAnswer {
	p, ok := r.pending[s.Job]
	if !ok {
		failCase("%s: nothing to join", where)
	}
	delete(r.pending, s.Job)
	select {
	case <-p.done:
	case <-time.After(60 * time.Second):
		failCase("%s: the async submit did not finish within 60 s", where)
	}
	if p.err != nil {
		panic(p.err)
	}
	r.finish(s, where, p.wire, true, s.Job)
	if p.name != "" {
		r.captured[p.name] = p.wire
	}
	return p.wire
}

// syncStep calls the public QueryDocument; the transport supplies the raw
// answer for §7.
func (r *jobCaseRunner) syncStep(s jobKitStep, where string) wireAnswer {
	cl := r.client(s, where)
	file, instr, opts := r.request(s, where)
	before := len(r.transport.snapshot())
	boundarySeam.Lock()
	prev := newBoundary
	b := r.boundary(s)
	newBoundary = func() string { return b }
	res, err := cl.QueryDocument(context.Background(), file, instr, opts...)
	newBoundary = prev
	boundarySeam.Unlock()
	answers := r.transport.snapshot()[before:]
	if len(answers) != 1 {
		failCase("%s: QueryDocument made %d requests, not 1", where, len(answers))
	}
	w := answers[0].wire
	if v, has := s.Expect["status"]; has {
		var want int
		_ = json.Unmarshal(v, &want)
		if want == 200 && (err != nil || res == nil) {
			failCase("%s: QueryDocument: %v", where, err)
		}
		var de *Error
		if want != 200 && (!errors.As(err, &de) || de.Status != want) {
			failCase("%s: QueryDocument returned %v, want status %d", where, err, want)
		}
	}
	r.finish(s, where, w, false, "")
	return w
}

func (r *jobCaseRunner) poll(s jobKitStep, where string) wireAnswer {
	path := r.pathOf(s, where)
	if s.Until == nil {
		w := r.sendJob(s, where, http.MethodGet, path)
		r.finish(s, where, w, true, "")
		return w
	}
	// Poll until the named status, sleeping each Retry-After in real time.
	var observed []string
	slept := 0
	var w wireAnswer
	for index := 0; ; index++ {
		w = r.sendJob(s, where, http.MethodGet, path)
		checkJobInvariants(fmt.Sprintf("%s poll %d", where, index), w)
		status, isStr := fieldOf(jsonValue(w.body), "status").(string)
		if w.status != 200 || !isStr {
			break
		}
		observed = append(observed, status)
		if status == *s.Until {
			break
		}
		if status == "succeeded" || status == "failed" || status == "cancelled" {
			failCase("%s: reached %s before %s", where, status, *s.Until)
		}
		ra := w.header.Get("Retry-After")
		if ra == "" {
			failCase("%s: a %s poll without Retry-After", where, status)
		}
		n, err := strconv.Atoi(ra)
		if err != nil {
			failCase("%s: Retry-After %q", where, ra)
		}
		if slept+n > pollUntilSeconds {
			failCase("%s: %s not reached within %d s", where, *s.Until, pollUntilSeconds)
		}
		time.Sleep(time.Duration(n) * time.Second)
		slept += n
	}
	r.finish(s, where, w, true, "")
	if raw, has := s.Expect["states"]; has {
		var allowed []string
		if err := json.Unmarshal(raw, &allowed); err != nil {
			failCase("%s: states: %v", where, err)
		}
		if states := dedupeStates(observed); !isSubsequence(states, allowed) {
			failCase("%s: observed states %v are not a subsequence of %v", where, states, allowed)
		}
	}
	return w
}

// wait calls the public Wait and reads its outcome the way the kit's
// waitForJob reports it: succeeded or cancelled (a Job), failed (the job's
// error, not retryable), error (any other *Error) or timeout (*WaitError).
func (r *jobCaseRunner) wait(s jobKitStep, where string) {
	for k := range s.Expect {
		if k != "outcome" && k != "reason" {
			failCase("%s: a wait step checks only outcome and reason, not %s", where, k)
		}
	}
	cl := r.client(s, where)
	jobID, ok := r.jobs[s.Job]
	if !ok {
		failCase("%s: job %s was never bound by a 2xx submit", where, s.Job)
	}
	ctx := context.Background()
	if s.TimeoutSeconds != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(*s.TimeoutSeconds*float64(time.Second)))
		defer cancel()
	}
	before := len(r.transport.snapshot())
	job, err := cl.Wait(ctx, jobID)
	seen := r.transport.snapshot()
	var outcome string
	var reason any
	var we *WaitError
	var de *Error
	switch {
	case err == nil:
		if job.JobID != jobID {
			failCase("%s: Wait returned job %q, want %q", where, job.JobID, jobID)
		}
		switch job.Status {
		case JobStatusSucceeded:
			if job.Result == nil {
				failCase("%s: succeeded without a result", where)
			}
		case JobStatusCancelled:
			if job.Result != nil {
				failCase("%s: cancelled with a result", where)
			}
		default:
			failCase("%s: Wait returned non-terminal status %q", where, job.Status)
		}
		outcome = string(job.Status)
	case errors.As(err, &we):
		if we.JobID != jobID {
			failCase("%s: WaitError.JobID %q, want %q", where, we.JobID, jobID)
		}
		outcome = "timeout"
	case errors.As(err, &de):
		if de.JobID != jobID {
			failCase("%s: wait error without the job id", where)
		}
		last := seen[len(seen)-1].wire
		failedJob := last.status == 200 && fieldOf(jsonValue(last.body), "status") == "failed"
		outcome = "error"
		if failedJob {
			outcome = "failed"
			if de.Retryable {
				failCase("%s: a failed job's error is retryable", where)
			}
		}
		reason = de.Reason
	default:
		failCase("%s: Wait returned %T: %v", where, err, err)
	}
	for _, x := range seen[before:] {
		if x.method != http.MethodGet {
			failCase("%s: Wait sent %s, never anything but GET", where, x.method)
		}
	}
	if v, has := s.Expect["outcome"]; has && !sameJSON(outcome, jsonValue(v)) {
		failCase("%s: outcome %s, expected %s", where, outcome, v)
	}
	if v, has := s.Expect["reason"]; has && !sameJSON(reason, jsonValue(v)) {
		failCase("%s: reason %v, expected %s", where, reason, v)
	}
}

// TestContractJobCases runs every stub-target kit v3 job case with via
// submit, poll, cancel or wait as its own subtest. contract.yml fails unless
// the PASS count equals the jq count of the same selection.
func TestContractJobCases(t *testing.T) {
	skipWithoutStub(t)
	kit := loadJobKit(t)
	cases := selectedJobCases(kit)
	if len(cases) == 0 {
		t.Fatal("no stub job case selected")
	}
	base := stubURL(t)
	run := runToken(t)
	ran := 0
	for _, c := range cases {
		t.Run(c.ID, func(t *testing.T) {
			newJobCaseRunner(t, kit, c, run, base).runCase()
			ran++
		})
	}
	if ran != len(cases) {
		t.Fatalf("ran %d of %d job cases: a subtest stopped before its last assertion", ran, len(cases))
	}
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
