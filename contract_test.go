package docql

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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

// stubURL returns the base URL of the running stub, exported by
// scripts/stub-up.sh --run. With DOCQL_REQUIRE_STUB=1 a missing stub fails the
// run; otherwise the contract cases skip so plain unit runs stay green.
func stubURL(t *testing.T) string {
	t.Helper()
	if u := os.Getenv("DOCQL_STUB_URL"); u != "" {
		return u
	}
	if os.Getenv("DOCQL_REQUIRE_STUB") == "1" {
		t.Fatal("DOCQL_REQUIRE_STUB=1 but DOCQL_STUB_URL is not set: run the cases through scripts/stub-up.sh --run")
	}
	t.Skip("DOCQL_STUB_URL is not set: run through scripts/stub-up.sh --run to check the shared cases")
	return ""
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

// TestContractSDKCases runs every via:sdk case of the vendored shared list
// against the digest-pinned stub, through the same QueryDocument path the
// tracer fixed.
func TestContractSDKCases(t *testing.T) {
	kit := loadSDKCases(t)
	for i := range kit.Cases {
		c := &kit.Cases[i]
		if c.Via != "sdk" {
			continue
		}
		t.Run(c.ID, func(t *testing.T) {
			runSDKCase(t, kit, c)
		})
	}
}

func runSDKCase(t *testing.T, kit *sdkCasesFile, c *contractCase) {
	t.Helper()
	base := stubURL(t)

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
		return
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
}
