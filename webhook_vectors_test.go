package docql

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The shared console webhook vectors (D-16). testdata/webhook-vectors.json is
// a byte copy of docql-console packages/webhooks/webhook-vectors.json
// (version 1, frozen in 45.1-01): the console's real signer and payload
// builders made it, so these cases prove the helper agrees with what the
// console sends. A regenerated console file changes both pinned values, and
// the copy here must then be replaced byte for byte.
const (
	webhookVectorsSHA256    = "63bbaeb4063ebcbd98fd4535121d29333589efddc85a6a9c6eb5419e7b186bbd"
	webhookVectorsCaseCount = 123
)

// webhookVectorFile is the part of the vector file the Go tests read.
type webhookVectorFile struct {
	Format  string          `json:"format"`
	Version int             `json:"version"`
	Cases   []webhookVector `json:"cases"`
}

type webhookVector struct {
	ID               string      `json:"id"`
	Secrets          []string    `json:"secrets"`
	ToleranceSeconds float64     `json:"tolerance_seconds"`
	Now              int64       `json:"now"`
	Headers          [][2]string `json:"headers"`
	BodyBase64       string      `json:"body_base64"`
	Expect           struct {
		Result      string `json:"result"`
		Reason      string `json:"reason"`
		Argument    string `json:"argument"`
		SecretIndex *int   `json:"secret_index"`
		Event       *struct {
			ID               string          `json:"id"`
			Type             string          `json:"type"`
			TimestampEpochMs int64           `json:"timestamp_epoch_ms"`
			JobID            *string         `json:"job_id"`
			Status           *string         `json:"status"`
			Data             json.RawMessage `json:"data"`
		} `json:"event"`
	} `json:"expect"`
}

func readWebhookVectors(t *testing.T) ([]byte, *webhookVectorFile) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "webhook-vectors.json"))
	if err != nil {
		t.Fatalf("read testdata/webhook-vectors.json: %v", err)
	}
	var file webhookVectorFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("decode testdata/webhook-vectors.json: %v", err)
	}
	return raw, &file
}

// TestWebhookVectorFilePin: the vendored copy is byte-identical to the frozen
// console file (sha256), version 1, with exactly the pinned number of unique
// cases.
func TestWebhookVectorFilePin(t *testing.T) {
	raw, file := readWebhookVectors(t)
	if got := hex.EncodeToString(sha256Sum(raw)); got != webhookVectorsSHA256 {
		t.Fatalf("WEBHOOK_VECTORS_MISMATCH: vendored sha256 %s, pinned %s", got, webhookVectorsSHA256)
	}
	if file.Format != "docql-webhook-vectors" || file.Version != 1 {
		t.Fatalf("format %q version %d, want docql-webhook-vectors version 1", file.Format, file.Version)
	}
	ids := map[string]bool{}
	for _, c := range file.Cases {
		ids[c.ID] = true
	}
	if len(file.Cases) != webhookVectorsCaseCount || len(ids) != webhookVectorsCaseCount {
		t.Fatalf("%d cases (%d unique ids), want exactly %d", len(file.Cases), len(ids), webhookVectorsCaseCount)
	}
}

// TestWebhookVectors runs every shared case as a subtest through the
// verifyWebhookAt seam at the case's own clock.
func TestWebhookVectors(t *testing.T) {
	_, file := readWebhookVectors(t)
	if len(file.Cases) != webhookVectorsCaseCount {
		t.Fatalf("%d cases, want %d", len(file.Cases), webhookVectorsCaseCount)
	}
	for _, c := range file.Cases {
		t.Run(c.ID, func(t *testing.T) {
			t.Parallel()
			runWebhookVector(t, c)
		})
	}
}

func runWebhookVector(t *testing.T, c webhookVector) {
	body, err := base64.StdEncoding.DecodeString(c.BodyBase64)
	if err != nil {
		t.Fatalf("body_base64: %v", err)
	}
	// Raw keys, never the canonicalizing Header.Add: the case's name spelling
	// and multiplicity reach the helper unchanged.
	header := http.Header{}
	for _, pair := range c.Headers {
		header[pair[0]] = append(header[pair[0]], pair[1])
	}
	var opts []WebhookOption
	// The 300 s cases pass no option, so the default is what they exercise.
	if c.ToleranceSeconds != 300 {
		opts = append(opts, WithWebhookTolerance(time.Duration(c.ToleranceSeconds*float64(time.Second))))
	}
	event, err := verifyWebhookAt(time.Unix(c.Now, 0), body, header, c.Secrets, opts...)

	switch c.Expect.Result {
	case "event":
		if err != nil {
			t.Fatalf("err = %v, want an event", err)
		}
		checkWebhookEvent(t, event, c)
	case "webhook_error":
		var we *WebhookError
		if !errors.As(err, &we) {
			t.Fatalf("err = %v (%T), want *WebhookError", err, err)
		}
		if we.Reason != c.Expect.Reason {
			t.Fatalf("Reason = %q, want %q", we.Reason, c.Expect.Reason)
		}
		if !reflect.DeepEqual(event, WebhookEvent{}) {
			t.Errorf("event = %+v next to an error, want the zero WebhookEvent", event)
		}
		assertNoWebhookEcho(t, err.Error(), c)
	case "validation_error":
		if err == nil {
			t.Fatal("err = nil, want a validation error")
		}
		var we *WebhookError
		if errors.As(err, &we) {
			t.Fatalf("err = %v is a *WebhookError, want a validation sentinel", err)
		}
		msg := err.Error()
		switch c.Expect.Argument {
		case "tolerance":
			if !errors.Is(err, ErrInvalidWebhookTolerance) {
				t.Fatalf("err = %v, want errors.Is ErrInvalidWebhookTolerance", err)
			}
			if errors.Is(err, ErrInvalidWebhookSecret) {
				t.Fatalf("err = %v also matches ErrInvalidWebhookSecret", err)
			}
		case "secrets":
			if !errors.Is(err, ErrInvalidWebhookSecret) {
				t.Fatalf("err = %v, want errors.Is ErrInvalidWebhookSecret", err)
			}
			if errors.Is(err, ErrInvalidWebhookTolerance) {
				t.Fatalf("err = %v also matches ErrInvalidWebhookTolerance", err)
			}
		default:
			t.Fatalf("unknown expect.argument %q", c.Expect.Argument)
		}
		if c.Expect.SecretIndex != nil {
			if want := fmt.Sprintf("secret %d", *c.Expect.SecretIndex); !strings.Contains(msg, want) {
				t.Errorf("message %q does not name %q", msg, want)
			}
		}
		assertNoWebhookEcho(t, msg, c)
	default:
		t.Fatalf("unknown expect.result %q", c.Expect.Result)
	}
}

func checkWebhookEvent(t *testing.T, event WebhookEvent, c webhookVector) {
	t.Helper()
	want := c.Expect.Event
	if want == nil {
		t.Fatal("event case without expect.event")
	}
	if event.ID != want.ID || event.Type != want.Type {
		t.Errorf("ID, Type = %q, %q; want %q, %q", event.ID, event.Type, want.ID, want.Type)
	}
	if event.Timestamp.Location() != time.UTC {
		t.Errorf("Timestamp location = %v, want UTC", event.Timestamp.Location())
	}
	if got := event.Timestamp.UnixMilli(); got != want.TimestampEpochMs {
		t.Errorf("Timestamp.UnixMilli() = %d, want %d", got, want.TimestampEpochMs)
	}
	if wantJobID := stringOrEmpty(want.JobID); event.JobID != wantJobID {
		t.Errorf("JobID = %q, want %q", event.JobID, wantJobID)
	}
	if wantStatus := JobStatus(stringOrEmpty(want.Status)); event.Status != wantStatus {
		t.Errorf("Status = %q, want %q", event.Status, wantStatus)
	}
	var wantData map[string]any
	dec := json.NewDecoder(bytes.NewReader(want.Data))
	dec.UseNumber()
	if err := dec.Decode(&wantData); err != nil {
		t.Fatalf("expect.event.data: %v", err)
	}
	if event.Data == nil {
		t.Fatal("Data is nil, want a map")
	}
	if !reflect.DeepEqual(event.Data, wantData) {
		t.Errorf("Data = %#v, want %#v", event.Data, wantData)
	}
}

func stringOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// windows returns every size-byte window of s.
func windows(s string, size int) []string {
	var out []string
	for i := 0; i+size <= len(s); i++ {
		out = append(out, s[i:i+size])
	}
	return out
}

// assertNoWebhookEcho: an error text never carries a secret (its text or any
// 6-character window of its base64 part) or a piece of a received signature.
func assertNoWebhookEcho(t *testing.T, msg string, c webhookVector) {
	t.Helper()
	for i, secret := range c.Secrets {
		text := strings.Trim(secret, " \t\r\n")
		if len(text) > len("whsec_") && strings.Contains(msg, text) {
			t.Errorf("message carries secret %d", i+1)
		}
		b64 := secret
		if _, after, found := strings.Cut(secret, "whsec_"); found {
			b64 = after
		}
		for _, w := range windows(b64, 6) {
			if strings.Contains(msg, w) {
				t.Errorf("message carries a piece of secret %d", i+1)
				break
			}
		}
	}
	for _, pair := range c.Headers {
		if !strings.EqualFold(pair[0], "webhook-signature") {
			continue
		}
		for _, entry := range strings.Split(pair[1], " ") {
			for _, w := range windows(strings.TrimPrefix(entry, "v1,"), 6) {
				if strings.Contains(msg, w) {
					t.Errorf("message carries a piece of a signature")
					break
				}
			}
		}
	}
}
