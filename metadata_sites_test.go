package docql

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestErrorMetadataNeverNil (SSDK-02): an *Error the SDK builds itself, not
// through decodeError, still carries a non-nil empty Metadata — an unusable
// 2xx on QueryDocument, an unusable job body, and a failed job without an
// error object.
func TestErrorMetadataNeverNil(t *testing.T) {
	answer := func(body string) *Client {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, body)
		}))
		t.Cleanup(srv.Close)
		c, err := NewClient(WithAPIKey("k"), WithAPIURL(srv.URL))
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		return c
	}
	ctx := context.Background()
	cases := []struct {
		name string
		call func() error
	}{
		{"unusable 2xx on QueryDocument", func() error {
			_, err := answer("<html>ok</html>").QueryDocument(ctx, FileBytes([]byte("a,b\n"), "a.csv"), Query("q"))
			return err
		}},
		{"unusable job body on GetJob", func() error {
			_, err := answer(`{"status":"queued"}`).GetJob(ctx, "job-1")
			return err
		}},
		{"failed job without an error on Wait", func() error {
			_, err := answer(`{"job_id":"job-1","status":"failed"}`).Wait(ctx, "job-1")
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var de *Error
			if err := c.call(); !errors.As(err, &de) {
				t.Fatalf("err = %v, want *Error", err)
			}
			if de.Metadata == nil {
				t.Fatal("Metadata is nil, want a non-nil empty map")
			}
			if len(de.Metadata) != 0 {
				t.Fatalf("Metadata = %v, want empty", de.Metadata)
			}
		})
	}
}
