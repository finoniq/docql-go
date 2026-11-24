package docql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// TestErrorStrings pins the two Error() forms (D-09) against the vendored
// cases and the stub-shaped envelope.
func TestErrorStrings(t *testing.T) {
	// The 429 envelope with Retry-After 5.
	h := http.Header{}
	h.Set("Retry-After", "5")
	err := decodeError(429, h, envelopeBody("service_overloaded", "slow down", strPtr("req-429")), "k")
	want := "429 service_overloaded: slow down (request_id=req-429, retry_after=5s)"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}

	// The vendored framework 404.
	cases := loadDecodeCases(t)
	c := findDecodeCase(t, cases, "framework-404")
	framework := decodeError(c.Status, caseHeader(c), []byte(c.BodyUTF8), "k")
	wantFramework := `404 (non-DocQL response): {"detail":"Not Found"} (request_id=00000000-0000-4000-8000-000000000404)`
	if got := framework.Error(); got != wantFramework {
		t.Errorf("Error() = %q, want %q", got, wantFramework)
	}

	// An empty 502 body.
	empty := decodeError(502, nil, nil, "k")
	if got := empty.Error(); got != "502 (non-DocQL response): (empty body)" {
		t.Errorf("Error() = %q, want the empty-body form", got)
	}

	// The vendored 524 HTML: the snippet is whitespace-collapsed, so no CR or
	// LF survives, and it is at most 200 characters.
	edge := findDecodeCase(t, cases, "edge-524-html")
	timeout := decodeError(edge.Status, caseHeader(edge), []byte(edge.BodyUTF8), "k")
	text := timeout.Error()
	if strings.ContainsAny(text, "\r\n") {
		t.Errorf("Error() contains a raw CR or LF: %q", text)
	}
	idx := strings.Index(text, "): ")
	if idx < 0 {
		t.Fatalf("Error() has no snippet separator: %q", text)
	}
	if snippet := text[idx+len("): "):]; len([]rune(snippet)) > 200 {
		t.Errorf("snippet is %d characters, want at most 200", len([]rune(snippet)))
	}
}

// TestErrorTypedNil guards the Go typed-nil pitfall (SC3): a success returns a
// nil error interface, and every failure carries the right concrete type.
func TestErrorTypedNil(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data": {"answer": 42}, "metadata": {"request_id": "ok-1", "page_count": 2}}`)
	}))
	defer srv.Close()
	client, cerr := NewClient(WithAPIKey("k"), WithAPIURL(srv.URL))
	if cerr != nil {
		t.Fatalf("NewClient: %v", cerr)
	}

	// Success: the error interface must compare equal to nil, not hold a
	// typed nil *Error.
	successErr := func() error {
		_, err := client.QueryDocument(context.Background(), FileBytes([]byte("f"), "f.pdf"), Query("q"))
		return err
	}()
	if successErr != nil {
		t.Fatalf("success path returned a non-nil error: %v", successErr)
	}

	// A 422 answer: errors.As finds *Error, no sentinel matches.
	unprocessable := func() error {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"error_info":"nope","metadata":{"reason":"missing_query_or_prompt","request_id":"req-422"}}`)
		}))
		defer s.Close()
		c, cerr := NewClient(WithAPIKey("k"), WithAPIURL(s.URL))
		if cerr != nil {
			t.Fatalf("NewClient: %v", cerr)
		}
		_, err := c.QueryDocument(context.Background(), FileBytes([]byte("f"), "f.pdf"), Query("q"))
		return err
	}()
	var de *Error
	if !errors.As(unprocessable, &de) {
		t.Fatalf("422: errors.As found no *Error in %v", unprocessable)
	}
	for _, sentinel := range []error{ErrMissingAPIKey, ErrInvalidAPIURL, ErrInvalidTimeout, ErrNilContext, ErrNoInstruction, ErrNoFile, ErrMissingFilename} {
		if errors.Is(unprocessable, sentinel) {
			t.Errorf("422 error unexpectedly matches %v", sentinel)
		}
	}

	// A refused connection: *ConnectionError, never *Error.
	refused := func() error {
		c, cerr := NewClient(WithAPIKey("k"), WithAPIURL("http://127.0.0.1:1"))
		if cerr != nil {
			t.Fatalf("NewClient: %v", cerr)
		}
		_, err := c.QueryDocument(context.Background(), FileBytes([]byte("f"), "f.pdf"), Query("q"))
		return err
	}()
	var ce *ConnectionError
	if !errors.As(refused, &ce) {
		t.Fatalf("refused connection: errors.As found no *ConnectionError in %v", refused)
	}
	if errors.As(refused, &de) {
		t.Error("refused connection: errors.As found a *Error")
	}

	// A zero File with a valid instruction: the ErrNoFile sentinel, never a
	// transport or server error type.
	validation := func() error {
		_, err := client.QueryDocument(context.Background(), File{}, Query("q"))
		return err
	}()
	if !errors.Is(validation, ErrNoFile) {
		t.Fatalf("zero File: errors.Is(err, ErrNoFile) failed: %v", validation)
	}
	if errors.As(validation, &de) || errors.As(validation, &ce) {
		t.Errorf("zero File error carries a transport/server type: %v", validation)
	}
}

// TestErrorMetadata pins Error.Metadata (SSDK-02, D-06, D-12): the raw
// metadata object of the error body, numbers kept as json.Number, the API key
// scrubbed from every key and string value, never nil, never in Error().
func TestErrorMetadata(t *testing.T) {
	const key = "dql_metadataTestKey0123456789abcd_AbC123"

	t.Run("int64 maximum stays exact", func(t *testing.T) {
		body := `{"error_info":"too large","metadata":{"bytes":9223372036854775807,"limit":5000000,"reason":"spreadsheet_too_large","request_id":"r"}}`
		e := decodeError(422, nil, []byte(body), key)
		if e.Reason != "spreadsheet_too_large" {
			t.Fatalf("Reason = %q, want spreadsheet_too_large", e.Reason)
		}
		n, ok := e.Metadata["bytes"].(json.Number)
		if !ok {
			t.Fatalf("Metadata[bytes] = %T, want json.Number", e.Metadata["bytes"])
		}
		v, err := n.Int64()
		if err != nil || v != math.MaxInt64 {
			t.Fatalf("Metadata[bytes].Int64() = %d, %v; want %d", v, err, int64(math.MaxInt64))
		}
		if n.String() != "9223372036854775807" {
			t.Fatalf("Metadata[bytes] = %s, want 9223372036854775807", n)
		}
		if got := e.Metadata["limit"]; got != json.Number("5000000") {
			t.Fatalf("Metadata[limit] = %#v, want json.Number(5000000)", got)
		}
		if e.Metadata["reason"] != "spreadsheet_too_large" || e.Metadata["request_id"] != "r" {
			t.Fatalf("Metadata = %v, want reason and request_id kept", e.Metadata)
		}
	})

	t.Run("too_many_pages fills limit and pages", func(t *testing.T) {
		body := `{"metadata":{"limit":2,"pages":3,"reason":"too_many_pages","request_id":"r"}}`
		e := decodeError(422, nil, []byte(body), key)
		if e.Reason != "too_many_pages" {
			t.Fatalf("Reason = %q, want too_many_pages", e.Reason)
		}
		want := map[string]any{"limit": json.Number("2"), "pages": json.Number("3"), "reason": "too_many_pages", "request_id": "r"}
		if !reflect.DeepEqual(e.Metadata, want) {
			t.Fatalf("Metadata = %#v, want %#v", e.Metadata, want)
		}
	})

	t.Run("filled for every reason and without one", func(t *testing.T) {
		e := decodeError(422, nil, []byte(`{"metadata":{"reason":"document_too_complex","request_id":"r","depth":7}}`), key)
		if e.Reason != "document_too_complex" || e.Metadata["depth"] != json.Number("7") {
			t.Fatalf("document_too_complex: Reason %q, Metadata %v", e.Reason, e.Metadata)
		}
		e = decodeError(500, nil, []byte(`{"metadata":{"cells":558}}`), key)
		if e.Reason != "" || e.Metadata["cells"] != json.Number("558") {
			t.Fatalf("no reason: Reason %q, Metadata %v", e.Reason, e.Metadata)
		}
	})

	t.Run("the API key is scrubbed from keys and string values", func(t *testing.T) {
		body := `{"metadata":{"reason":"spreadsheet_too_large","request_id":"r","` + key + `":"x",` +
			`"note":"echo ` + key + ` back","nested":{"list":["` + key + `",1,true,null],"k-` + key + `":{"v":"` + key + `"}}}}`
		e := decodeError(422, nil, []byte(body), key)
		if dump := fmt.Sprintf("%#v", e.Metadata); strings.Contains(dump, key) {
			t.Fatalf("Metadata still carries the key: %s", dump)
		}
		if e.Metadata["***"] != "x" || e.Metadata["note"] != "echo *** back" {
			t.Fatalf("Metadata = %v, want the key replaced by ***", e.Metadata)
		}
		nested, ok := e.Metadata["nested"].(map[string]any)
		if !ok {
			t.Fatalf("Metadata[nested] = %T, want an object", e.Metadata["nested"])
		}
		wantList := []any{"***", json.Number("1"), true, nil}
		if !reflect.DeepEqual(nested["list"], wantList) {
			t.Fatalf("nested list = %#v, want %#v", nested["list"], wantList)
		}
		if inner, ok := nested["k-***"].(map[string]any); !ok || inner["v"] != "***" {
			t.Fatalf("nested object = %#v, want key and value scrubbed", nested)
		}
	})

	t.Run("Error() is the same with and without metadata", func(t *testing.T) {
		plain := decodeError(422, nil, []byte(`{"error_info":"too large","metadata":{"reason":"spreadsheet_too_large","request_id":"r"}}`), key)
		rich := decodeError(422, nil, []byte(`{"error_info":"too large","metadata":{"reason":"spreadsheet_too_large","request_id":"r","cells":558,"limit":557,"forged":"\nlevel=error FORGED"}}`), key)
		if plain.Error() != rich.Error() {
			t.Fatalf("Error() differs: %q vs %q", plain.Error(), rich.Error())
		}
		if text := rich.Error(); strings.Contains(text, "558") || strings.Contains(text, "FORGED") {
			t.Fatalf("Error() carries metadata: %q", text)
		}
	})

	t.Run("non-nil and empty without a metadata object", func(t *testing.T) {
		for name, body := range map[string]string{
			"empty body":       "",
			"non-JSON body":    "<html>524</html>",
			"no metadata":      `{"error_info":"x"}`,
			"metadata array":   `{"metadata":[1,2]}`,
			"metadata null":    `{"metadata":null}`,
			"metadata string":  `{"metadata":"reason"}`,
			"top-level array":  `[{"metadata":{"cells":1}}]`,
			"truncated object": `{"metadata":{"cells":1`,
		} {
			e := decodeError(502, nil, []byte(body), key)
			if e.Metadata == nil || len(e.Metadata) != 0 {
				t.Errorf("%s: Metadata = %#v, want a non-nil empty map", name, e.Metadata)
			}
		}
	})

	t.Run("parsed past the body cut", func(t *testing.T) {
		body := `{"error_info":"` + strings.Repeat("x", 3000) + `","metadata":{"cells":558,"limit":557,"reason":"spreadsheet_too_large","request_id":"r"}}`
		e := decodeError(422, nil, []byte(body), key)
		if got := len([]rune(e.Body)); got != errorBodyKeepChars {
			t.Fatalf("Body kept %d characters, want %d", got, errorBodyKeepChars)
		}
		if strings.Contains(e.Body, "cells") {
			t.Fatal("the metadata should lie past the cut Body")
		}
		if e.Metadata["cells"] != json.Number("558") || e.Metadata["limit"] != json.Number("557") {
			t.Fatalf("Metadata = %v, want cells 558 and limit 557", e.Metadata)
		}
	})
}

// TestErrorMetadataRoutes: every route that answers an error fills Metadata
// through decodeError — QueryDocument, SubmitDocument, GetJob, Wait,
// CancelJob — and a failed job's embedded error carries its own metadata.
func TestErrorMetadataRoutes(t *testing.T) {
	const refusal = `{"error_info":"too large","metadata":{"cells":558,"limit":557,"reason":"spreadsheet_too_large","request_id":"r"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, refusal)
	}))
	defer srv.Close()
	client, err := NewClient(WithAPIKey("k"), WithAPIURL(srv.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx := context.Background()
	file := FileBytes([]byte("a,b\n"), "a.csv")
	calls := map[string]func() error{
		"QueryDocument":  func() error { _, e := client.QueryDocument(ctx, file, Query("q")); return e },
		"SubmitDocument": func() error { _, e := client.SubmitDocument(ctx, file, Query("q")); return e },
		"GetJob":         func() error { _, e := client.GetJob(ctx, "job-1"); return e },
		"Wait":           func() error { _, e := client.Wait(ctx, "job-1"); return e },
		"CancelJob":      func() error { _, e := client.CancelJob(ctx, "job-1"); return e },
	}
	for name, call := range calls {
		var de *Error
		if err := call(); !errors.As(err, &de) {
			t.Fatalf("%s: err = %v, want *Error", name, err)
		}
		if de.Metadata["cells"] != json.Number("558") || de.Metadata["limit"] != json.Number("557") {
			t.Errorf("%s: Metadata = %v, want cells 558 and limit 557", name, de.Metadata)
		}
	}

	failed := `{"job_id":"job-2","status":"failed","error":{"status_code":422,"error_info":"too large",` +
		`"metadata":{"cells":558,"limit":557,"reason":"spreadsheet_too_large","request_id":"r"}}}`
	job, err := parseJob(200, http.Header{}, []byte(failed), "k", "")
	if err != nil || job.Error == nil {
		t.Fatalf("parseJob: job %+v, err %v", job, err)
	}
	if job.Error.Reason != "spreadsheet_too_large" || job.Error.Metadata["cells"] != json.Number("558") {
		t.Fatalf("failed job error: Reason %q, Metadata %v", job.Error.Reason, job.Error.Metadata)
	}
}

// TestErrorMetadataEmbeddedJobScrub (45.1 IN-04): a failed job's embedded
// error goes through parseJob and decodeError, and an API key the server
// echoes in a metadata key and inside a metadata value comes back as ***,
// with the key nowhere in the job's error.
func TestErrorMetadataEmbeddedJobScrub(t *testing.T) {
	const key = "dql_InEmbeddedJobScrubKey7Qz4Wv9Xc2Lp_Ab12Cd"
	failed := `{"job_id":"job-scrub","status":"failed","error":{"status_code":422,` +
		`"error_info":"refused for ` + key + `","metadata":{"reason":"invalid_params","request_id":"r-scrub",` +
		`"` + key + `":"as a key","echo":"sent ` + key + ` back","nested":{"list":["` + key + `",1]}}}}`
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, failed)
	}))
	defer srv.Close()
	client, err := NewClient(WithAPIKey(key), WithAPIURL(srv.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	job, err := client.GetJob(context.Background(), "job-scrub")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if gotPath != "/v1/jobs/job-scrub" {
		t.Fatalf("path = %q", gotPath)
	}
	var de *Error
	if job.Status != JobStatusFailed || !errors.As(job.Error, &de) {
		t.Fatalf("job = %+v, want a failed job with an *Error", job)
	}
	if de.JobID != "job-scrub" || de.Reason != "invalid_params" || de.Retryable {
		t.Errorf("Error = %+v", de)
	}
	if de.Metadata["***"] != "as a key" {
		t.Errorf("Metadata[***] = %v, want the echoed key replaced by ***", de.Metadata["***"])
	}
	if de.Metadata["echo"] != "sent *** back" {
		t.Errorf("Metadata[echo] = %v, want sent *** back", de.Metadata["echo"])
	}
	nested, _ := de.Metadata["nested"].(map[string]any)
	if list, _ := nested["list"].([]any); len(list) != 2 || list[0] != "***" || list[1] != json.Number("1") {
		t.Errorf("Metadata[nested] = %v", de.Metadata["nested"])
	}
	encoded, err := json.Marshal(de.Metadata)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	for name, text := range map[string]string{
		"metadata": string(encoded), "Message": de.Message, "Body": de.Body, "Error()": de.Error(),
		"RequestID": de.RequestID, "job": fmt.Sprintf("%+v", *job),
	} {
		if strings.Contains(text, key) {
			t.Errorf("%s carries the API key: %s", name, text)
		}
	}
}
