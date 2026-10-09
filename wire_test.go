package docql

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

// setBoundary pins newBoundary for one test; t.Cleanup restores it.
func setBoundary(t *testing.T, boundary string) {
	t.Helper()
	prev := newBoundary
	newBoundary = func() string { return boundary }
	t.Cleanup(func() { newBoundary = prev })
}

// TestWireGolden proves the request is byte-identical with the golden fixture
// (locked GO-03 wire rule): CreatePart framing with the pinned boundary, an
// exact Content-Length and no chunked framing, and exactly the three
// per-request headers (D-14, D-15).
func TestWireGolden(t *testing.T) {
	kit := loadGoldenKit(t)
	for _, gc := range kit.Cases {
		if gc.ID != "ascii-query" {
			continue // later plans extend coverage to the remaining golden cases
		}
		t.Run(gc.ID, func(t *testing.T) {
			want, err := os.ReadFile(filepath.Join("contract", filepath.FromSlash(gc.Bin)))
			if err != nil {
				t.Fatalf("read golden fixture: %v", err)
			}
			fileData, err := base64.StdEncoding.DecodeString(gc.FileBase64)
			if err != nil {
				t.Fatalf("decode golden file bytes: %v", err)
			}

			setBoundary(t, kit.Boundary)

			type recorded struct {
				header           http.Header
				body             []byte
				contentLength    int64
				transferEncoding []string
			}
			got := make(chan *recorded, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				got <- &recorded{
					header:           r.Header.Clone(),
					body:             body,
					contentLength:    r.ContentLength,
					transferEncoding: r.TransferEncoding,
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":{},"metadata":{"request_id":"wire-req-1"}}`))
			}))
			defer srv.Close()

			client, err := NewClient(WithAPIKey("wire-test-key"), WithAPIURL(srv.URL))
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			if _, err := client.QueryDocument(context.Background(),
				FileBytes(fileData, gc.Filename),
				Query(gc.Query),
				WithFilename(gc.Filename),
			); err != nil {
				t.Fatalf("QueryDocument: %v", err)
			}
			rec := <-got

			if !bytes.Equal(rec.body, want) {
				t.Fatalf("request body differs from %s: got %d bytes, want %d", gc.Bin, len(rec.body), len(want))
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
		})
	}
}
