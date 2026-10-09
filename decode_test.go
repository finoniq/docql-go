package docql

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// decodeCase mirrors one entry of contract/sdk-cases.json decodeCases.
type decodeCase struct {
	ID       string            `json:"id"`
	Status   int               `json:"status"`
	Headers  map[string]string `json:"headers"`
	BodyUTF8 string            `json:"bodyUtf8"`
	Expect   struct {
		Status    int     `json:"status"`
		Reason    *string `json:"reason"`
		RequestID *string `json:"requestId"`
	} `json:"expect"`
}

// loadDecodeCases reads the vendored decode cases, relative to the package
// directory like every other kit consumer.
func loadDecodeCases(t *testing.T) []decodeCase {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("contract", "sdk-cases.json"))
	if err != nil {
		t.Fatalf("read contract/sdk-cases.json: %v", err)
	}
	var kit struct {
		DecodeCases []decodeCase `json:"decodeCases"`
	}
	if err := json.Unmarshal(raw, &kit); err != nil {
		t.Fatalf("decode contract/sdk-cases.json: %v", err)
	}
	return kit.DecodeCases
}

func findDecodeCase(t *testing.T, cases []decodeCase, id string) decodeCase {
	t.Helper()
	for _, c := range cases {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("decode case %q not found in the vendored kit", id)
	return decodeCase{}
}

// caseHeader builds the http.Header of a vendored case; a case without a
// headers object stays nil, like the wire gives it.
func caseHeader(c decodeCase) http.Header {
	if c.Headers == nil {
		return nil
	}
	h := http.Header{}
	for k, v := range c.Headers {
		h.Set(k, v)
	}
	return h
}

// strPtr is a test helper for optional JSON string expectations.
func strPtr(s string) *string { return &s }

// envelopeBody builds a DocQL error envelope like the stub's sendEnvelopeError
// output.
func envelopeBody(reason, errorInfo string, requestID *string) []byte {
	md := map[string]any{"reason": reason}
	if requestID != nil {
		md["request_id"] = *requestID
	}
	doc := map[string]any{"error_info": errorInfo, "status_code": 503, "metadata": md}
	b, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return b
}

// TestDecodeCases runs every vendored decode case through decodeError and
// checks the semantic expectation (status, reason, request id).
func TestDecodeCases(t *testing.T) {
	cases := loadDecodeCases(t)
	if len(cases) == 0 {
		t.Fatal("the vendored kit has no decodeCases")
	}
	for _, c := range cases {
		t.Run(c.ID, func(t *testing.T) {
			err := decodeError(c.Status, caseHeader(c), []byte(c.BodyUTF8), "k")
			if err.Status != c.Expect.Status {
				t.Errorf("Status = %d, want %d", err.Status, c.Expect.Status)
			}
			wantReason := ""
			if c.Expect.Reason != nil {
				wantReason = *c.Expect.Reason
			}
			if err.Reason != wantReason {
				t.Errorf("Reason = %q, want %q", err.Reason, wantReason)
			}
			wantID := ""
			if c.Expect.RequestID != nil {
				wantID = *c.Expect.RequestID
			}
			if err.RequestID != wantID {
				t.Errorf("RequestID = %q, want %q", err.RequestID, wantID)
			}
		})
	}
}

// TestDecodeRetryable pins the retryable set: only 429 and 503 (D-09).
func TestDecodeRetryable(t *testing.T) {
	for _, tc := range []struct {
		status    int
		retryable bool
	}{
		{429, true},
		{503, true},
		{400, false},
		{401, false},
		{404, false},
		{413, false},
		{415, false},
		{422, false},
		{500, false},
		{502, false},
		{504, false},
		{524, false},
	} {
		err := decodeError(tc.status, nil, envelopeBody("service_overloaded", "busy", strPtr("req-1")), "k")
		if err.Retryable != tc.retryable {
			t.Errorf("status %d: Retryable = %v, want %v", tc.status, err.Retryable, tc.retryable)
		}
	}
}

// TestDecodeRetryAfter covers both Retry-After forms (D-09): delta-seconds as
// is, the HTTP-date rounded up against an injected now and clamped at 0, and
// unparsable or overflowing values as 0.
func TestDecodeRetryAfter(t *testing.T) {
	nowWhole := time.Date(2026, 10, 8, 12, 0, 1, 0, time.UTC)
	nowSub := time.Date(2026, 10, 8, 12, 0, 0, 800000000, time.UTC)
	httpDate := func(second, minute, hour int) string {
		return time.Date(2026, 10, 8, hour, minute, second, 0, time.UTC).Format(http.TimeFormat)
	}

	// Delta-seconds: trimmed and passed through as-is.
	if got := parseRetryAfter("5", nowWhole); got != 5*time.Second {
		t.Errorf(`parseRetryAfter("5") = %v, want 5s`, got)
	}
	if got := parseRetryAfter(" 7 ", nowWhole); got != 7*time.Second {
		t.Errorf(`parseRetryAfter(" 7 ") = %v, want 7s`, got)
	}

	// An HTTP-date exactly 30 s after an injected whole-second now → 30s.
	if got := parseRetryAfter(httpDate(31, 0, 12), nowWhole); got != 30*time.Second {
		t.Errorf("HTTP-date 30.0 s ahead = %v, want 30s", got)
	}
	// The same 30 s target seen from 0.8 s later is 29.2 s away → ceil → 30s.
	if got := parseRetryAfter(httpDate(30, 0, 12), nowSub); got != 30*time.Second {
		t.Errorf("HTTP-date 29.2 s ahead = %v, want 30s (ceil)", got)
	}
	// A date in the past clamps at 0.
	if got := parseRetryAfter(httpDate(0, 59, 11), nowSub); got != 0 {
		t.Errorf("past HTTP-date = %v, want 0", got)
	}

	// Unparsable and overflowing values are 0.
	for _, v := range []string{"soon", "-1", "", "99999999999999999999", "9999999999"} {
		if got := parseRetryAfter(v, nowWhole); got != 0 {
			t.Errorf("parseRetryAfter(%q) = %v, want 0", v, got)
		}
	}

	// A Retry-After on a 503 is parsed too, through the full decode.
	h := http.Header{}
	h.Set("Retry-After", "7")
	err := decodeError(503, h, envelopeBody("upstream_unavailable", "down", strPtr("req-3")), "k")
	if err.RetryAfter != 7*time.Second {
		t.Errorf("decodeError RetryAfter = %v, want 7s", err.RetryAfter)
	}
}

// TestDecodeNonEnvelope keeps Reason empty for bodies without a DocQL envelope
// and takes the request id from the X-Request-Id header.
func TestDecodeNonEnvelope(t *testing.T) {
	h := http.Header{}
	h.Set("X-Request-Id", "hdr-req-1")
	for name, body := range map[string][]byte{
		"numeric-reason": []byte(`{"metadata": {"reason": 5}}`),
		"array":          []byte(`[1, 2]`),
		"html":           []byte(`<html><body>nope</body></html>`),
	} {
		err := decodeError(500, h, body, "k")
		if err.Reason != "" {
			t.Errorf("%s: Reason = %q, want empty", name, err.Reason)
		}
		if err.RequestID != "hdr-req-1" {
			t.Errorf("%s: RequestID = %q, want the header value", name, err.RequestID)
		}
	}
}

// TestDecodeParseResult pins the 2xx rules (D-11): unusable bodies raise the
// named *Error instead of an empty result, absent or null data is {}, unknown
// metadata fields survive, numbers decode as json.Number, page counts are
// exact integers only, and the request id falls back to the header, then "".
func TestDecodeParseResult(t *testing.T) {
	t.Run("unusable-non-json", func(t *testing.T) {
		for _, body := range []string{"<html>", "[]"} {
			res, err := parseResult(200, nil, []byte(body), "k")
			if res != nil {
				t.Fatalf("body %q: result is not nil", body)
			}
			var de *Error
			if !errors.As(err, &de) {
				t.Fatalf("body %q: error is not *Error: %v", body, err)
			}
			if de.Status != 200 || de.Reason != "" || de.Retryable {
				t.Errorf("body %q: got %+v", body, de)
			}
			if de.Message != "unexpected non-JSON response" {
				t.Errorf("body %q: Message = %q", body, de.Message)
			}
		}
	})

	t.Run("data-not-an-object", func(t *testing.T) {
		_, err := parseResult(200, nil, []byte(`{"data": [1]}`), "k")
		var de *Error
		if !errors.As(err, &de) {
			t.Fatalf("error is not *Error: %v", err)
		}
		if de.Message != "unexpected response: data is not a JSON object" {
			t.Errorf("Message = %q", de.Message)
		}
	})

	t.Run("missing-or-null-data-is-empty-object", func(t *testing.T) {
		for _, body := range []string{`{}`, `{"data": null}`} {
			res, err := parseResult(200, nil, []byte(body), "k")
			if err != nil {
				t.Fatalf("body %s: %v", body, err)
			}
			if !bytes.Equal(res.Data, []byte("{}")) {
				t.Errorf("body %s: Data = %s, want {}", body, res.Data)
			}
		}
	})

	t.Run("unknown-metadata-kept-and-numbers-json-number", func(t *testing.T) {
		body := []byte(`{"data": {"a": 1}, "metadata": {"request_id": "r1", "future_field": "kept", "ratio": 1.5}}`)
		res, err := parseResult(200, nil, body, "k")
		if err != nil {
			t.Fatalf("%v", err)
		}
		if res.Metadata["future_field"] != "kept" {
			t.Errorf("future_field = %v, want kept", res.Metadata["future_field"])
		}
		if res.Metadata["request_id"] != "r1" {
			t.Errorf("request_id = %v, want r1", res.Metadata["request_id"])
		}
		if n, ok := res.Metadata["ratio"].(json.Number); !ok || n.String() != "1.5" {
			t.Errorf("ratio = %#v, want json.Number(\"1.5\")", res.Metadata["ratio"])
		}
	})

	t.Run("page-count-exact-int-only", func(t *testing.T) {
		for _, tc := range []struct {
			value string
			want  bool
		}{
			{`true`, false},
			{`3.0`, false},
			{`"3"`, false},
			{`9223372036854775808`, false}, // overflows int
			{`3`, true},
		} {
			body := []byte(`{"data": {}, "metadata": {"page_count": ` + tc.value + `}}`)
			res, err := parseResult(200, nil, body, "k")
			if err != nil {
				t.Fatalf("page_count %s: %v", tc.value, err)
			}
			if tc.want {
				if res.PageCount == nil || *res.PageCount != 3 {
					t.Errorf("page_count %s: got %v, want 3", tc.value, res.PageCount)
				}
			} else if res.PageCount != nil {
				t.Errorf("page_count %s: got %v, want nil", tc.value, *res.PageCount)
			}
		}
	})

	t.Run("request-id-fallback", func(t *testing.T) {
		fromBody, err := parseResult(200, nil, []byte(`{"data": {}, "metadata": {"request_id": "body-id"}}`), "k")
		if err != nil || fromBody.RequestID != "body-id" {
			t.Errorf("from body: %v, %q", err, fromBody.RequestID)
		}
		h := http.Header{}
		h.Set("X-Request-Id", "header-id")
		fromHeader, err := parseResult(200, h, []byte(`{"data": {}}`), "k")
		if err != nil || fromHeader.RequestID != "header-id" {
			t.Errorf("from header: %v, %q", err, fromHeader.RequestID)
		}
		neither, err := parseResult(200, nil, []byte(`{"data": {}}`), "k")
		if err != nil || neither.RequestID != "" {
			t.Errorf("neither: %v, %q", err, neither.RequestID)
		}
	})
}

// TestDecodeBodyLimits pins the body and message limits (D-09, D-13): 2048
// characters kept, the cut at a rune boundary, invalid UTF-8 repaired, and the
// key scrubbed from every stored surface before anything is cut.
func TestDecodeBodyLimits(t *testing.T) {
	err := decodeError(500, nil, bytes.Repeat([]byte("x"), 5000), "k")
	if got := len(err.Body); got != 2048 {
		t.Errorf("ASCII body kept %d characters, want 2048", got)
	}

	// A 3000-rune Thai body keeps exactly 2048 runes and stays valid UTF-8.
	thai := strings.Repeat("ก", 3000)
	err = decodeError(500, nil, []byte(thai), "k")
	if runes := []rune(err.Body); len(runes) != 2048 {
		t.Errorf("Thai body kept %d runes, want 2048", len(runes))
	}
	if !utf8.ValidString(err.Body) {
		t.Error("Thai body is not valid UTF-8 after the cut")
	}

	// Invalid UTF-8 bytes become U+FFFD before the scrub and the cut.
	err = decodeError(500, nil, []byte{'a', 0xff, 0xfe, 'b'}, "k")
	if !strings.Contains(err.Body, string(utf8.RuneError)) {
		t.Errorf("invalid UTF-8 not repaired: %q", err.Body)
	}

	// The key is scrubbed from Body, Message and Error(), even when the
	// server echoes it in error_info and the raw body.
	key := "secret-key-123"
	body := []byte(`{"error_info":"key ` + key + ` is invalid","echo":"` + key + `","metadata":{"reason":"invalid_api_key","request_id":"req-9"}}`)
	err = decodeError(401, nil, body, key)
	for name, surface := range map[string]string{"Body": err.Body, "Message": err.Message, "Error()": err.Error()} {
		if strings.Contains(surface, key) {
			t.Errorf("%s still contains the key: %q", name, surface)
		}
		if !strings.Contains(surface, "***") {
			t.Errorf("%s lost the *** marker: %q", name, surface)
		}
	}
}
