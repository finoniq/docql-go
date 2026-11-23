package docql

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Every kit v5 spreadsheet case on both routes against the digest-pinned stub
// (GO-07, SSDK-02). contract/sheet-cases.json lists 26 cases, each with routes
// ["sync", "jobs"], so TestContractSheetCases runs 52 subtests named
// <id>@<route>. sync calls QueryDocument; jobs calls SubmitDocument and then
// Wait, and a refusal must come from SubmitDocument itself.
//
// The SDK sends opaque bytes: the upload name goes out byte for byte ("a.csv "
// keeps its trailing space) and the SDK never checks a file's type, name or
// size. Whether a file is refused, and whether a value equal to its limit
// passes, is the server's answer.
//
// Before any request, a fixture's vendored bytes must match the sha256 the
// vendored contract/manifest.json lists for it, so a drifted fixture fails the
// case instead of testing different bytes. For a 422 the error's Metadata must
// hold every expect.metadata entry as the same json.Number (the SSDK-02
// proof).

// sheetFile is the file of a sheet case: a vendored fixture path, or inline
// base64 contents (csv-empty sends zero bytes).
type sheetFile struct {
	Name    string  `json:"name"`
	Fixture string  `json:"fixture"`
	Base64  *string `json:"base64"`
}

type sheetRequest struct {
	File  sheetFile `json:"file"`
	Query *string   `json:"query"`
	Mode  *string   `json:"mode"`
}

type sheetExpect struct {
	Status       int            `json:"status"`
	Reason       *string        `json:"reason"`
	FileMimetype *string        `json:"file_mimetype"`
	PageCount    *int           `json:"page_count"`
	PagesOCR     *int           `json:"pages_ocr"`
	Metadata     map[string]any `json:"metadata"`
}

type sheetCase struct {
	ID      string       `json:"id"`
	Target  string       `json:"target"`
	Routes  []string     `json:"routes"`
	Request sheetRequest `json:"request"`
	Expect  sheetExpect  `json:"expect"`
}

type sheetCasesFile struct {
	Cases []sheetCase `json:"cases"`
}

// knownSheetRoutes are the routes this runner replays; any other route in the
// kit fails the run instead of silently disappearing.
var knownSheetRoutes = []string{"sync", "jobs"}

// loadSheetCases decodes contract/sheet-cases.json with numbers kept as
// json.Number, so expect.metadata compares exactly.
func loadSheetCases(t *testing.T) *sheetCasesFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("contract", "sheet-cases.json"))
	if err != nil {
		t.Fatalf("read contract/sheet-cases.json: %v", err)
	}
	var kit sheetCasesFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&kit); err != nil {
		t.Fatalf("decode contract/sheet-cases.json: %v", err)
	}
	return &kit
}

// sheetFixtureBytes returns the upload bytes of a sheet case. A fixture must
// match its sha256 in root/contract/manifest.json first.
func sheetFixtureBytes(root string, f sheetFile) ([]byte, error) {
	if f.Base64 != nil {
		return base64.StdEncoding.DecodeString(*f.Base64)
	}
	if f.Fixture == "" {
		return nil, fmt.Errorf("case file %q has neither fixture nor base64", f.Name)
	}
	data, err := os.ReadFile(filepath.Join(root, "contract", filepath.FromSlash(f.Fixture)))
	if err != nil {
		return nil, err
	}
	manifestRaw, err := os.ReadFile(filepath.Join(root, "contract", "manifest.json"))
	if err != nil {
		return nil, err
	}
	var manifest struct {
		Files map[string]string `json:"files"`
	}
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		return nil, fmt.Errorf("decode manifest.json: %w", err)
	}
	declared := manifest.Files[f.Fixture]
	actual := hex.EncodeToString(sha256Sum(data))
	if declared != actual {
		return nil, fmt.Errorf("CONTRACT_KIT_MISMATCH %s: vendored sha256 %s, manifest %s", f.Fixture, actual, declared)
	}
	return data, nil
}

// TestSheetFixtureDriftFails: a fixture whose bytes drifted from the manifest
// fails before any request is sent.
func TestSheetFixtureDriftFails(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "contract", "sheets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "contract", "sheets", "plain.csv"), []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := fmt.Sprintf(`{"files":{"sheets/plain.csv":%q}}`, hex.EncodeToString(sha256Sum([]byte("original\n"))))
	if err := os.WriteFile(filepath.Join(root, "contract", "manifest.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := sheetFixtureBytes(root, sheetFile{Name: "plain.csv", Fixture: "sheets/plain.csv"})
	if err == nil || !strings.Contains(err.Error(), "CONTRACT_KIT_MISMATCH sheets/plain.csv") {
		t.Fatalf("drifted fixture: err = %v, want CONTRACT_KIT_MISMATCH sheets/plain.csv", err)
	}
}

// TestContractSheetCases runs every sheet case once per route. The ran-count
// assertion after the loop keeps a half-skipped list from going green.
func TestContractSheetCases(t *testing.T) {
	skipWithoutStub(t)
	kit := loadSheetCases(t)
	// sheet-cases.json boots the stub with the job-cases.json recipe and its
	// keys.
	key := loadJobKit(t).Keys.Valid
	want := 0
	for _, c := range kit.Cases {
		for _, r := range c.Routes {
			if !slices.Contains(knownSheetRoutes, r) {
				t.Fatalf("sheet-cases.json case %s carries an unknown route %q", c.ID, r)
			}
			want++
		}
	}
	if want == 0 {
		t.Fatal("sheet-cases.json lists no case runs")
	}
	ran := 0
	for i := range kit.Cases {
		c := &kit.Cases[i]
		for _, route := range c.Routes {
			t.Run(c.ID+"@"+route, func(t *testing.T) {
				runSheetCase(t, key, c, route)
				ran++
			})
		}
	}
	if ran != want {
		t.Fatalf("ran %d of %d sheet case runs: a subtest stopped before its last assertion", ran, want)
	}
}

func runSheetCase(t *testing.T, key string, c *sheetCase, route string) {
	t.Helper()
	base := stubURL(t)
	data, err := sheetFixtureBytes(".", c.Request.File)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	client, err := NewClient(WithAPIKey(key), WithAPIURL(base))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.Request.Query == nil {
		t.Fatal("case carries no query")
	}
	var opts []QueryOption
	if c.Request.Mode != nil {
		opts = append(opts, WithMode(Mode(*c.Request.Mode)))
	}
	// The name is sent verbatim: never trimmed or normalised (SSDK-03).
	file := FileBytes(data, c.Request.File.Name)
	instr := Query(*c.Request.Query)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var res *QueryResult
	switch route {
	case "sync":
		res, err = client.QueryDocument(ctx, file, instr, opts...)
	case "jobs":
		var job *Job
		job, err = client.SubmitDocument(ctx, file, instr, opts...)
		if err == nil {
			if job == nil || job.JobID == "" {
				t.Fatal("SubmitDocument returned no job")
			}
			if c.Expect.Status != 200 {
				t.Fatalf("expected SubmitDocument to refuse with %d, got job %s (%s)", c.Expect.Status, job.JobID, job.Status)
			}
			job, err = client.Wait(ctx, job.JobID)
			if err != nil {
				t.Fatalf("Wait: %v", err)
			}
			if job.Status != JobStatusSucceeded || job.Result == nil {
				t.Fatalf("job %s ended %s without a result", job.JobID, job.Status)
			}
			res = job.Result
		}
	default:
		t.Fatalf("unknown route %q", route)
	}

	if c.Expect.Status == 200 {
		if err != nil {
			t.Fatalf("expected status 200, got error: %v", err)
		}
		if res.PageCount == nil || c.Expect.PageCount == nil || *res.PageCount != *c.Expect.PageCount {
			t.Fatalf("page_count = %v, want %v", deref(res.PageCount), deref(c.Expect.PageCount))
		}
		if res.PagesOCR == nil || c.Expect.PagesOCR == nil || *res.PagesOCR != *c.Expect.PagesOCR {
			t.Fatalf("pages_ocr = %v, want %v", deref(res.PagesOCR), deref(c.Expect.PagesOCR))
		}
		if c.Expect.FileMimetype == nil {
			t.Fatal("a 200 sheet case must name its file_mimetype")
		}
		if got := res.Metadata["file_mimetype"]; got != *c.Expect.FileMimetype {
			t.Fatalf("file_mimetype = %v, want %q", got, *c.Expect.FileMimetype)
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
	if c.Expect.Reason == nil || de.Reason != *c.Expect.Reason {
		t.Fatalf("reason = %q, want %v", de.Reason, deref(c.Expect.Reason))
	}
	if de.Metadata == nil {
		t.Fatal("Metadata is nil")
	}
	if got := de.Metadata["reason"]; got != *c.Expect.Reason {
		t.Fatalf("Metadata[\"reason\"] = %v, want %q", got, *c.Expect.Reason)
	}
	for k, wantV := range c.Expect.Metadata {
		wantN, ok := wantV.(json.Number)
		if !ok {
			t.Fatalf("expect.metadata[%q] = %T, want a number in the kit", k, wantV)
		}
		gotV, present := de.Metadata[k]
		if !present {
			t.Fatalf("Metadata has no %q: %v", k, de.Metadata)
		}
		gotN, ok := gotV.(json.Number)
		if !ok {
			t.Fatalf("Metadata[%q] = %T(%v), want json.Number", k, gotV, gotV)
		}
		if gotN.String() != wantN.String() {
			t.Fatalf("Metadata[%q] = %s, want %s", k, gotN, wantN)
		}
	}
}

// deref renders an optional value for a failure message.
func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}
