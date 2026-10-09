package docql

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// goldenKit mirrors the parts of contract/golden.json the wire test reads.
type goldenKit struct {
	Boundary    string `json:"boundary"`
	ContentType string `json:"contentType"`
	Cases       []struct {
		ID         string `json:"id"`
		Bin        string `json:"bin"`
		Filename   string `json:"filename"`
		FileBase64 string `json:"fileBase64"`
		Query      string `json:"query"`
		Prompt     string `json:"prompt"`
		Mode       string `json:"mode"`
	} `json:"cases"`
}

func loadGoldenKit(t *testing.T) *goldenKit {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("contract", "golden.json"))
	if err != nil {
		t.Fatalf("read contract/golden.json: %v", err)
	}
	var kit goldenKit
	if err := json.Unmarshal(raw, &kit); err != nil {
		t.Fatalf("decode contract/golden.json: %v", err)
	}
	return &kit
}

// goldenCase returns the case with the given id.
func goldenCase(t *testing.T, kit *goldenKit, id string) *struct {
	ID         string `json:"id"`
	Bin        string `json:"bin"`
	Filename   string `json:"filename"`
	FileBase64 string `json:"fileBase64"`
	Query      string `json:"query"`
	Prompt     string `json:"prompt"`
	Mode       string `json:"mode"`
} {
	t.Helper()
	for i := range kit.Cases {
		if kit.Cases[i].ID == id {
			return &kit.Cases[i]
		}
	}
	t.Fatalf("no golden case %q", id)
	return nil
}

// goldenCaseBytes decodes a case's golden fixture and file bytes.
func goldenCaseBytes(t *testing.T, gc *struct {
	ID         string `json:"id"`
	Bin        string `json:"bin"`
	Filename   string `json:"filename"`
	FileBase64 string `json:"fileBase64"`
	Query      string `json:"query"`
	Prompt     string `json:"prompt"`
	Mode       string `json:"mode"`
}) (want, fileData []byte) {
	t.Helper()
	want, err := os.ReadFile(filepath.Join("contract", filepath.FromSlash(gc.Bin)))
	if err != nil {
		t.Fatalf("read golden fixture: %v", err)
	}
	fileData, err = base64.StdEncoding.DecodeString(gc.FileBase64)
	if err != nil {
		t.Fatalf("decode golden file bytes: %v", err)
	}
	return want, fileData
}

// setBoundary pins newBoundary for one test; t.Cleanup restores it.
func setBoundary(t *testing.T, boundary string) {
	t.Helper()
	prev := newBoundary
	newBoundary = func() string { return boundary }
	t.Cleanup(func() { newBoundary = prev })
}

// assertGoldenRequest sends one QueryDocument call with the pinned golden
// boundary and proves the request is byte-identical with the fixture: exact
// body bytes, golden Content-Type, exact ContentLength, no chunked framing,
// and exactly the per-request headers (D-14, D-15).
func assertGoldenRequest(t *testing.T, kit *goldenKit, want []byte, file File, instr Instruction, callOpts ...QueryOption) {
	t.Helper()
	setBoundary(t, kit.Boundary)

	srv, got := serveCapture(t)
	client, err := NewClient(WithAPIKey("wire-test-key"), WithAPIURL(srv.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.QueryDocument(context.Background(), file, instr, callOpts...); err != nil {
		t.Fatalf("QueryDocument: %v", err)
	}
	rec := <-got

	if !bytes.Equal(rec.body, want) {
		t.Fatalf("request body differs from the golden fixture: got %d bytes, want %d", len(rec.body), len(want))
	}
	if ct := rec.header.Get("Content-Type"); ct != kit.ContentType {
		t.Fatalf("Content-Type = %q, want %q", ct, kit.ContentType)
	}
	if rec.contentLength != int64(len(want)) {
		t.Fatalf("ContentLength = %d, want %d", rec.contentLength, len(want))
	}
	if len(rec.transferEncoding) != 0 {
		t.Fatalf("TransferEncoding = %v, want none: the body has a known length", rec.transferEncoding)
	}
	if key := rec.header.Get("X-API-Key"); key != "wire-test-key" {
		t.Fatalf("X-API-Key = %q, want the key set for this request", key)
	}
	if rec.header.Get("X-Request-Id") != "" {
		t.Fatal("X-Request-Id must never be sent (D-15)")
	}
	for k := range rec.header {
		if strings.HasPrefix(k, "X-Docql-") {
			t.Fatalf("header %s must never be sent (D-15)", k)
		}
	}
	ua := regexp.MustCompile(`^docql-go/0\.1\.0 \([^;()]+; [^/()]+/[^()]+\)$`)
	if !ua.MatchString(rec.header.Get("User-Agent")) {
		t.Fatalf("User-Agent = %q, want the docql-go/0.1.0 (goVersion; goos/goarch) form (D-14)",
			rec.header.Get("User-Agent"))
	}
}

// TestWireGolden proves every golden case is matched byte for byte through
// the bytes, path and reader inputs, all with a known Content-Length.
func TestWireGolden(t *testing.T) {
	kit := loadGoldenKit(t)
	for i := range kit.Cases {
		gc := &kit.Cases[i]
		t.Run(gc.ID, func(t *testing.T) {
			want, fileData := goldenCaseBytes(t, gc)
			var instr Instruction
			switch {
			case gc.Prompt != "":
				instr = Prompt(gc.Prompt)
			default:
				instr = Query(gc.Query)
			}
			opts := []QueryOption{WithFilename(gc.Filename)}
			if gc.Mode != "" {
				opts = append(opts, WithMode(Mode(gc.Mode)))
			}
			assertGoldenRequest(t, kit, want, FileBytes(fileData, gc.Filename), instr, opts...)
		})
	}

	// The path input: the part name comes from the basename.
	t.Run("ascii-query-path", func(t *testing.T) {
		gc := goldenCase(t, kit, "ascii-query")
		want, fileData := goldenCaseBytes(t, gc)
		document := filepath.Join(t.TempDir(), "invoice.pdf")
		if err := os.WriteFile(document, fileData, 0o600); err != nil {
			t.Fatalf("write temp document: %v", err)
		}
		assertGoldenRequest(t, kit, want, FilePath(document), Query(gc.Query))
	})

	// The reader input: the part name is the constructor's name argument.
	t.Run("ascii-query-reader", func(t *testing.T) {
		gc := goldenCase(t, kit, "ascii-query")
		want, fileData := goldenCaseBytes(t, gc)
		assertGoldenRequest(t, kit, want, FileReader(bytes.NewReader(fileData), "invoice.pdf"), Query(gc.Query))
	})
}
