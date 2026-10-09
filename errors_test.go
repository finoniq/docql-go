package docql

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
