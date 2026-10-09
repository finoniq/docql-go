package docql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// caseFileSpec is one file input of a contract case: base64 contents, or a
// byte repeated length times.
type caseFileSpec struct {
	Name       string `json:"name"`
	Base64     string `json:"base64"`
	RepeatByte string `json:"repeatByte"`
	Length     int64  `json:"length"`
}

// caseRaw is the raw-replay request of a via:raw case: a fixed body sent with
// a fixed content type, decoded through the same pure functions the SDK uses.
type caseRaw struct {
	ContentType string `json:"contentType"`
	BodyUtf8    string `json:"bodyUtf8"`
}

// caseRequest carries query and prompt as pointers, so the empty query of the
// missing-query case stays distinct from an absent field.
type caseRequest struct {
	File   caseFileSpec `json:"file"`
	Query  *string      `json:"query"`
	Prompt *string      `json:"prompt"`
	Mode   *string      `json:"mode"`
}

// caseExpect stays semantic: it never names the stub's extraction state or any
// stub-prefixed metadata key, so a cached repeat passes like a fresh call.
type caseExpect struct {
	Status            int     `json:"status"`
	Reason            *string `json:"reason"`
	PageCount         *int    `json:"pageCount"`
	PagesOCR          *int    `json:"pagesOcr"`
	RetryAfterSeconds *int    `json:"retryAfterSeconds"`
	RequestIDPresent  bool    `json:"requestIdPresent"`
}

type contractCase struct {
	ID      string      `json:"id"`
	Via     string      `json:"via"`
	Key     string      `json:"key"`
	Request caseRequest `json:"request"`
	Raw     *caseRaw    `json:"raw"`
	Expect  caseExpect  `json:"expect"`
}

type sdkCasesFile struct {
	Keys struct {
		Valid   string `json:"valid"`
		Invalid string `json:"invalid"`
	} `json:"keys"`
	Cases []contractCase `json:"cases"`
}

func loadSDKCases(t *testing.T) *sdkCasesFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("contract", "sdk-cases.json"))
	if err != nil {
		t.Fatalf("read contract/sdk-cases.json: %v", err)
	}
	var kit sdkCasesFile
	if err := json.Unmarshal(raw, &kit); err != nil {
		t.Fatalf("decode contract/sdk-cases.json: %v", err)
	}
	return &kit
}

// goldenFileBase64 returns the file bytes of one golden.json case (the
// document the Go-only live checks upload).
func goldenFileBase64(t *testing.T, id string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("contract", "golden.json"))
	if err != nil {
		t.Fatalf("read contract/golden.json: %v", err)
	}
	var golden struct {
		Cases []struct {
			ID         string `json:"id"`
			FileBase64 string `json:"fileBase64"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("decode contract/golden.json: %v", err)
	}
	for _, c := range golden.Cases {
		if c.ID == id {
			data, derr := base64.StdEncoding.DecodeString(c.FileBase64)
			if derr != nil {
				t.Fatalf("decode golden %s file: %v", id, derr)
			}
			return data
		}
	}
	t.Fatalf("golden case %q not found", id)
	return nil
}

// Kit integrity (GO-05): before any contract case runs, the stub must serve
// exactly the vendored kit. Three sha256 values must agree: the bytes
// GET /__stub/sdk-cases serves, the sdk-cases.json entry of the vendored
// manifest, and the vendored file itself. A mismatch — or a non-200, or an
// unreachable stub — fails every contract test in this binary. The comparison
// runs once per test binary (sync.Once) and its result is reused; it can
// fail, never silently skip.
var (
	kitOnce sync.Once
	kitErr  error
)

func checkKit(t *testing.T, base string) {
	t.Helper()
	kitOnce.Do(func() {
		kitErr = verifyKit(base)
	})
	if kitErr != nil {
		t.Fatalf("CONTRACT_KIT_MISMATCH: %v", kitErr)
	}
}

func verifyKit(base string) error {
	resp, err := http.Get(base + "/__stub/sdk-cases")
	if err != nil {
		return fmt.Errorf("GET %s/__stub/sdk-cases: %w", base, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read /__stub/sdk-cases: %w", err)
	}
	served := hex.EncodeToString(sha256Sum(body))
	manifestRaw, err := os.ReadFile(filepath.Join("contract", "manifest.json"))
	if err != nil {
		return fmt.Errorf("read contract/manifest.json: %w", err)
	}
	var manifest struct {
		Files map[string]string `json:"files"`
	}
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		return fmt.Errorf("decode contract/manifest.json: %w", err)
	}
	declared := manifest.Files["sdk-cases.json"]
	vendored, err := os.ReadFile(filepath.Join("contract", "sdk-cases.json"))
	if err != nil {
		return fmt.Errorf("read contract/sdk-cases.json: %w", err)
	}
	vendoredSum := hex.EncodeToString(sha256Sum(vendored))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET /__stub/sdk-cases: HTTP %d (served sha256 %s, manifest %s, vendored %s)", resp.StatusCode, served, declared, vendoredSum)
	}
	if served != declared || served != vendoredSum {
		return fmt.Errorf("sdk-cases.json sha256 mismatch: served %s, manifest %s, vendored %s", served, declared, vendoredSum)
	}
	return nil
}

func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// stubURL returns the base URL of the running stub, exported by
// scripts/stub-up.sh --run, after proving it serves the vendored kit. With
// DOCQL_REQUIRE_STUB=1 a missing stub fails the run; otherwise the contract
// cases skip so plain unit runs stay green.
func stubURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("DOCQL_STUB_URL")
	if u == "" {
		if os.Getenv("DOCQL_REQUIRE_STUB") == "1" {
			t.Fatal("DOCQL_REQUIRE_STUB=1 but DOCQL_STUB_URL is not set: run the cases through scripts/stub-up.sh --run")
		}
		t.Skip("DOCQL_STUB_URL is not set: run through scripts/stub-up.sh --run to check the shared cases")
		return ""
	}
	base := strings.TrimRight(u, "/")
	checkKit(t, base)
	return base
}

// skipWithoutStub ends the calling test when no stub is running: it skips, or
// fails under DOCQL_REQUIRE_STUB=1. A subtest's t.Skip unwinds the subtest
// goroutine, so the ran/skipped counters in the case-list parents never see
// it and their count assertion would fail a plain `go test ./...`; the parent
// has to decide once, before it starts any subtest.
func skipWithoutStub(t *testing.T) {
	t.Helper()
	if os.Getenv("DOCQL_STUB_URL") == "" {
		stubURL(t)
	}
}

func caseFileBytes(t *testing.T, f caseFileSpec) []byte {
	t.Helper()
	if f.Base64 != "" {
		data, err := base64.StdEncoding.DecodeString(f.Base64)
		if err != nil {
			t.Fatalf("decode case file base64: %v", err)
		}
		return data
	}
	return bytes.Repeat([]byte(f.RepeatByte), int(f.Length))
}

// TestContractKnownVia fails the run when the vendored list carries a case
// with an unknown via: a new case must be wired into a runner, never silently
// disappear.
func TestContractKnownVia(t *testing.T) {
	kit := loadSDKCases(t)
	var unknown []string
	sdk, raw := 0, 0
	for _, c := range kit.Cases {
		switch c.Via {
		case "sdk":
			sdk++
		case "raw":
			raw++
		default:
			unknown = append(unknown, c.ID)
		}
	}
	if len(unknown) != 0 {
		t.Fatalf("sdk-cases.json carries cases with an unknown via: %v", unknown)
	}
	if sdk+raw != len(kit.Cases) {
		t.Fatalf("sdk (%d) + raw (%d) cases != total %d", sdk, raw, len(kit.Cases))
	}
}

// countVia returns how many cases of the vendored list use one via.
func countVia(kit *sdkCasesFile, via string) int {
	n := 0
	for i := range kit.Cases {
		if kit.Cases[i].Via == via {
			n++
		}
	}
	return n
}

// TestContractSDKCases runs every via:sdk case of the vendored shared list
// against the digest-pinned stub, through the same QueryDocument path the
// tracer fixed. The ran-count assertion after the loop keeps a renumbered or
// half-skipped list from silently going green (T-12-21).
func TestContractSDKCases(t *testing.T) {
	skipWithoutStub(t)
	kit := loadSDKCases(t)
	want := countVia(kit, "sdk")
	ran, skipped := 0, 0
	for i := range kit.Cases {
		c := &kit.Cases[i]
		if c.Via != "sdk" {
			continue
		}
		t.Run(c.ID, func(t *testing.T) {
			if runSDKCase(t, kit, c) {
				ran++
			} else {
				skipped++
			}
		})
	}
	if ran+skipped != want {
		t.Fatalf("ran %d of %d sdk cases (%d skipped): a subtest stopped before its last assertion", ran+skipped, want, skipped)
	}
}

func runSDKCase(t *testing.T, kit *sdkCasesFile, c *contractCase) bool {
	t.Helper()
	base := stubURL(t)
	if base == "" {
		return false // skipped: no stub outside strict mode
	}

	key := kit.Keys.Valid
	if c.Key == "invalid" {
		key = kit.Keys.Invalid
	}
	client, err := NewClient(WithAPIKey(key), WithAPIURL(base))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	var instr Instruction
	switch {
	case c.Request.Query != nil:
		instr = Query(*c.Request.Query)
	case c.Request.Prompt != nil:
		instr = Prompt(*c.Request.Prompt)
	default:
		t.Fatal("case carries neither query nor prompt")
	}
	opts := []QueryOption{WithFilename(c.Request.File.Name)}
	if c.Request.Mode != nil {
		opts = append(opts, WithMode(Mode(*c.Request.Mode)))
	}

	res, err := client.QueryDocument(context.Background(),
		FileBytes(caseFileBytes(t, c.Request.File), c.Request.File.Name),
		instr,
		opts...,
	)

	if c.Expect.Status == 200 {
		if err != nil {
			t.Fatalf("expected status 200, got error: %v", err)
		}
		if res == nil {
			t.Fatal("expected a result")
		}
		if res.PageCount == nil || res.PagesOCR == nil {
			t.Fatalf("page counts must be present: page_count=%v pages_ocr=%v", res.PageCount, res.PagesOCR)
		}
		if *res.PageCount != *c.Expect.PageCount {
			t.Fatalf("page_count = %d, want %d", *res.PageCount, *c.Expect.PageCount)
		}
		if *res.PagesOCR != *c.Expect.PagesOCR {
			t.Fatalf("pages_ocr = %d, want %d", *res.PagesOCR, *c.Expect.PagesOCR)
		}
		if res.RequestID == "" {
			t.Fatal("request_id must be present on a 200 envelope")
		}
		return true
	}

	if err == nil {
		t.Fatalf("expected status %d, got a successful result", c.Expect.Status)
	}
	var de *Error
	if !errors.As(err, &de) {
		t.Fatalf("expected *docql.Error, got %T: %v", err, err)
	}
	if de.Status != c.Expect.Status {
		t.Fatalf("status = %d, want %d", de.Status, c.Expect.Status)
	}
	if c.Expect.Reason != nil && de.Reason != *c.Expect.Reason {
		t.Fatalf("reason = %q, want %q", de.Reason, *c.Expect.Reason)
	}
	if c.Expect.RetryAfterSeconds != nil &&
		de.RetryAfter != time.Duration(*c.Expect.RetryAfterSeconds)*time.Second {
		t.Fatalf("retry_after = %s, want %ds", de.RetryAfter, *c.Expect.RetryAfterSeconds)
	}
	if wantRetryable := c.Expect.Status == 429 || c.Expect.Status == 503; de.Retryable != wantRetryable {
		t.Fatalf("retryable = %v, want %v", de.Retryable, wantRetryable)
	}
	if c.Expect.RequestIDPresent && de.RequestID == "" {
		t.Fatal("request_id must be present")
	}
	return true
}

// replayClient is the plain HTTP client of the raw replays: it is test
// scaffolding, never the SDK's own client.
var replayClient = &http.Client{Timeout: 60 * time.Second}

// TestContractRawCases replays every via:raw case of the vendored list as a
// plain net/http POST and decodes the answer through the same decodeError /
// parseResult functions the SDK client uses, so the decoder is proven against
// live bytes, not only fixtures.
func TestContractRawCases(t *testing.T) {
	skipWithoutStub(t)
	kit := loadSDKCases(t)
	want := countVia(kit, "raw")
	ran, skipped := 0, 0
	for i := range kit.Cases {
		c := &kit.Cases[i]
		if c.Via != "raw" {
			continue
		}
		t.Run(c.ID, func(t *testing.T) {
			if runRawCase(t, kit, c) {
				ran++
			} else {
				skipped++
			}
		})
	}
	if ran+skipped != want {
		t.Fatalf("ran %d of %d raw cases (%d skipped): a subtest stopped before its last assertion", ran+skipped, want, skipped)
	}
}

func runRawCase(t *testing.T, kit *sdkCasesFile, c *contractCase) bool {
	t.Helper()
	base := stubURL(t)
	if base == "" {
		return false // skipped: no stub outside strict mode
	}
	if c.Raw == nil {
		t.Fatalf("raw case %q carries no raw request", c.ID)
	}

	key := kit.Keys.Valid
	if c.Key == "invalid" {
		key = kit.Keys.Invalid
	}
	req, err := http.NewRequest(http.MethodPost, base+queryPath, strings.NewReader(c.Raw.BodyUtf8))
	if err != nil {
		t.Fatalf("build raw request: %v", err)
	}
	req.Header.Set("Content-Type", c.Raw.ContentType)
	if c.Key != "missing" {
		req.Header.Set("X-API-Key", key)
	}
	resp, err := replayClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s%s: %v", base, queryPath, err)
	}
	defer resp.Body.Close()
	var body []byte
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		body, err = io.ReadAll(resp.Body)
	} else {
		body, err = io.ReadAll(io.LimitReader(resp.Body, errorBodyReadLimit))
	}
	if err != nil {
		t.Fatalf("read raw response: %v", err)
	}

	if c.Expect.Status == 200 {
		res, perr := parseResult(resp.StatusCode, resp.Header, body, key)
		if perr != nil {
			t.Fatalf("expected status 200, got decode error: %v", perr)
		}
		if res.PageCount == nil || res.PagesOCR == nil {
			t.Fatalf("page counts must be present: page_count=%v pages_ocr=%v", res.PageCount, res.PagesOCR)
		}
		if c.Expect.PageCount != nil && *res.PageCount != *c.Expect.PageCount {
			t.Fatalf("page_count = %d, want %d", *res.PageCount, *c.Expect.PageCount)
		}
		if c.Expect.PagesOCR != nil && *res.PagesOCR != *c.Expect.PagesOCR {
			t.Fatalf("pages_ocr = %d, want %d", *res.PagesOCR, *c.Expect.PagesOCR)
		}
		if res.RequestID == "" {
			t.Fatal("request_id must be present on a 200 envelope")
		}
		return true
	}

	de := decodeError(resp.StatusCode, resp.Header, body, key)
	if de.Status != c.Expect.Status {
		t.Fatalf("status = %d, want %d", de.Status, c.Expect.Status)
	}
	if c.Expect.Reason != nil && de.Reason != *c.Expect.Reason {
		t.Fatalf("reason = %q, want %q", de.Reason, *c.Expect.Reason)
	}
	if c.Expect.RetryAfterSeconds != nil &&
		de.RetryAfter != time.Duration(*c.Expect.RetryAfterSeconds)*time.Second {
		t.Fatalf("retry_after = %s, want %ds", de.RetryAfter, *c.Expect.RetryAfterSeconds)
	}
	if wantRetryable := c.Expect.Status == 429 || c.Expect.Status == 503; de.Retryable != wantRetryable {
		t.Fatalf("retryable = %v, want %v", de.Retryable, wantRetryable)
	}
	if c.Expect.RequestIDPresent && de.RequestID == "" {
		t.Fatal("request_id must be present")
	}
	return true
}

// assertDeadline checks the deadline-expiry error shape (D-10): a
// *ConnectionError with Timeout() and context.DeadlineExceeded in the chain.
func assertDeadline(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a timeout error, got a result")
	}
	var ce *ConnectionError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *docql.ConnectionError, got %T: %v", err, err)
	}
	if !ce.Timeout() {
		t.Fatalf("Timeout() = false for an expired deadline: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("errors.Is(err, context.DeadlineExceeded) = false: %v", err)
	}
}

// TestContractGoExtras runs the Go-only live checks against the pinned stub:
// live proof of D-02 (overall deadline, caller deadline, cleared injected
// Timeout), D-03 (concurrency) and D-06/D-07 (input kinds). Every subtest
// uses a distinct query string so the stub's extraction cache can never mask
// a difference, and no assertion reads the stub's extraction state or any
// stub-prefixed metadata key, so cached repeats pass like fresh calls.
func TestContractGoExtras(t *testing.T) {
	kit := loadSDKCases(t)
	base := stubURL(t)
	if base == "" {
		return // skipped: no stub outside strict mode
	}
	ctx := context.Background()
	okPDF := goldenFileBase64(t, "ascii-query")

	newClient := func(t *testing.T, opts ...Option) *Client {
		t.Helper()
		client, err := NewClient(append([]Option{WithAPIKey(kit.Keys.Valid), WithAPIURL(base)}, opts...)...)
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		return client
	}
	ran := 0

	// sdk-deadline (D-02/SC1): WithTimeout applies an overall per-call
	// deadline that cuts the stub's 3 s delay.
	t.Run("sdk-deadline", func(t *testing.T) {
		client := newClient(t, WithTimeout(time.Second))
		_, err := client.QueryDocument(ctx,
			FileBytes(okPDF, "invoice.pdf"),
			Query("go-extras: sdk deadline"),
			WithFilename("delay-3000.pdf"),
		)
		assertDeadline(t, err)
		ran++
	})

	// caller-deadline (D-02): an earlier deadline on the caller's context
	// wins over the SDK default.
	t.Run("caller-deadline", func(t *testing.T) {
		client := newClient(t) // default 660 s SDK deadline
		ctx2, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		_, err := client.QueryDocument(ctx2,
			FileBytes(okPDF, "invoice.pdf"),
			Query("go-extras: caller deadline"),
			WithFilename("delay-3000.pdf"),
		)
		assertDeadline(t, err)
		ran++
	})

	// injected-timeout-cleared (D-02/SC1): the injected client's own 1 s
	// Timeout is cleared, so the stub's 2 s delay completes with 200.
	t.Run("injected-timeout-cleared", func(t *testing.T) {
		client := newClient(t, WithHTTPClient(&http.Client{Timeout: time.Second}))
		res, err := client.QueryDocument(ctx,
			FileBytes(okPDF, "invoice.pdf"),
			Query("go-extras: injected timeout cleared"),
			WithFilename("delay-2000.pdf"),
		)
		if err != nil {
			t.Fatalf("the injected client's 1 s Timeout must not shorten the call: %v", err)
		}
		if res.PageCount == nil || *res.PageCount != 1 {
			t.Fatalf("page_count = %v, want 1", res.PageCount)
		}
		if res.RequestID == "" {
			t.Fatal("request_id must be present")
		}
		ran++
	})

	// canceled (D-10): a canceled context is a *ConnectionError with
	// context.Canceled in the chain and Timeout() false.
	t.Run("canceled", func(t *testing.T) {
		client := newClient(t)
		ctx2, cancel := context.WithCancel(ctx)
		time.AfterFunc(200*time.Millisecond, cancel)
		defer cancel()
		_, err := client.QueryDocument(ctx2,
			FileBytes(okPDF, "invoice.pdf"),
			Query("go-extras: canceled"),
			WithFilename("delay-3000.pdf"),
		)
		if err == nil {
			t.Fatal("expected a cancellation error, got a result")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("errors.Is(err, context.Canceled) = false: %v", err)
		}
		var ce *ConnectionError
		if !errors.As(err, &ce) {
			t.Fatalf("expected *docql.ConnectionError, got %T: %v", err, err)
		}
		if ce.Timeout() {
			t.Fatalf("Timeout() = true for a cancellation: %v", err)
		}
		ran++
	})

	// path-page-counts (D-06): a FilePath upload reports the filename knobs.
	t.Run("path-page-counts", func(t *testing.T) {
		client := newClient(t)
		p := filepath.Join(t.TempDir(), "pages-2_ocr-1.pdf")
		if err := os.WriteFile(p, okPDF, 0o600); err != nil {
			t.Fatalf("write temp file: %v", err)
		}
		res, err := client.QueryDocument(ctx, FilePath(p), Query("go-extras: path page counts"))
		if err != nil {
			t.Fatalf("FilePath call: %v", err)
		}
		if res.PageCount == nil || *res.PageCount != 2 {
			t.Fatalf("page_count = %v, want 2", res.PageCount)
		}
		if res.PagesOCR == nil || *res.PagesOCR != 1 {
			t.Fatalf("pages_ocr = %v, want 1", res.PagesOCR)
		}
		ran++
	})

	// reader-at-offset (D-07): a caller's *os.File is read from its current
	// position, never closed — it still answers Stat after the call.
	t.Run("reader-at-offset", func(t *testing.T) {
		client := newClient(t)
		p := filepath.Join(t.TempDir(), "positioned.pdf")
		if err := os.WriteFile(p, append([]byte("JUNKJOK"), okPDF...), 0o600); err != nil {
			t.Fatalf("write temp file: %v", err)
		}
		f, err := os.Open(p)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer f.Close()
		if _, err := f.Seek(7, io.SeekStart); err != nil {
			t.Fatalf("seek: %v", err)
		}
		res, err := client.QueryDocument(ctx, FileReader(f, "invoice.pdf"), Query("go-extras: reader at offset"))
		if err != nil {
			t.Fatalf("reader call from offset 7: %v", err)
		}
		if res.PageCount == nil || *res.PageCount != 1 {
			t.Fatalf("page_count = %v, want 1", res.PageCount)
		}
		if _, err := f.Stat(); err != nil {
			t.Fatalf("the caller's file must stay open after the call: %v", err)
		}
		ran++
	})

	// unmeasurable-reader (D-07): a reader whose length cannot be measured
	// is sent chunked and still completes.
	t.Run("unmeasurable-reader", func(t *testing.T) {
		client := newClient(t)
		reader := struct{ io.Reader }{bytes.NewReader(okPDF)} // Read only: length unknown
		res, err := client.QueryDocument(ctx, FileReader(reader, "invoice.pdf"), Query("go-extras: unmeasurable reader"))
		if err != nil {
			t.Fatalf("chunked upload of an unmeasurable reader: %v", err)
		}
		if res.PageCount == nil || *res.PageCount != 1 {
			t.Fatalf("page_count = %v, want 1", res.PageCount)
		}
		ran++
	})

	// path-too-large: the stub's 64 KiB cap answers 413 upload_too_large for
	// a 128 KiB FilePath upload.
	t.Run("path-too-large", func(t *testing.T) {
		client := newClient(t)
		p := filepath.Join(t.TempDir(), "big.pdf")
		if err := os.WriteFile(p, bytes.Repeat([]byte("A"), 131072), 0o600); err != nil {
			t.Fatalf("write temp file: %v", err)
		}
		_, err := client.QueryDocument(ctx, FilePath(p), Query("go-extras: path too large"))
		if err == nil {
			t.Fatal("expected 413 upload_too_large, got a result")
		}
		var de *Error
		if !errors.As(err, &de) {
			t.Fatalf("expected *docql.Error, got %T: %v", err, err)
		}
		if de.Status != http.StatusRequestEntityTooLarge || de.Reason != ReasonUploadTooLarge {
			t.Fatalf("got %d %q, want 413 upload_too_large", de.Status, de.Reason)
		}
		ran++
	})

	// concurrent (D-03): one client, eight goroutines, eight distinct
	// queries, all answering 200.
	t.Run("concurrent", func(t *testing.T) {
		client := newClient(t)
		errs := make([]error, 8)
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, errs[i] = client.QueryDocument(ctx,
					FileBytes(okPDF, "invoice.pdf"),
					Query(fmt.Sprintf("go-extras: concurrent %d", i)),
					WithFilename("invoice.pdf"),
				)
			}(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("goroutine %d: %v", i, err)
			}
		}
		ran++
	})

	if ran != 9 {
		t.Fatalf("ran %d of the 9 Go-only live checks: a subtest stopped before its last assertion", ran)
	}
}
