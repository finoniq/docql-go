package docql

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These tests cover the Go-only option, header and error contracts beyond the
// shared vectors (D-10, D-13, D-26, D-29). The HMACs here are computed with
// crypto/hmac in the test; the console vectors stay the cross-language truth.

// testWebhookKey returns n deterministic key bytes and the whsec_ secret for
// them.
func testWebhookKey(seed byte, n int) ([]byte, string) {
	key := make([]byte, n)
	for i := range key {
		key[i] = seed + byte(i)*7
	}
	return key, "whsec_" + base64.StdEncoding.EncodeToString(key)
}

func testWebhookSign(key []byte, id, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + timestamp + "."))
	mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

const (
	testWebhookNow  = int64(1795000000)
	testWebhookID   = "msg_2pQ7Xc9KfL3mN8vB"
	testWebhookBody = `{"id":"msg_2pQ7Xc9KfL3mN8vB","type":"job.succeeded","timestamp":"2026-11-17T03:04:05.678Z",` +
		`"data":{"job_id":"job_go_1","status":"succeeded","pages":3,"ratio":1.5}}`
)

// testDelivery is one signed delivery with raw lower-case header keys.
type testDelivery struct {
	key    []byte
	secret string
	sig    string
	body   []byte
	header http.Header
}

func newTestDelivery(body string) testDelivery {
	key, secret := testWebhookKey(3, 32)
	ts := strconv.FormatInt(testWebhookNow, 10)
	sig := testWebhookSign(key, testWebhookID, ts, []byte(body))
	return testDelivery{
		key:    key,
		secret: secret,
		sig:    sig,
		body:   []byte(body),
		header: http.Header{"webhook-id": {testWebhookID}, "webhook-timestamp": {ts}, "webhook-signature": {sig}},
	}
}

func (d testDelivery) verify(opts ...WebhookOption) (WebhookEvent, error) {
	return verifyWebhookAt(time.Unix(testWebhookNow, 0), d.body, d.header, []string{d.secret}, opts...)
}

func wantWebhookReason(t *testing.T, err error, reason string) {
	t.Helper()
	var we *WebhookError
	if !errors.As(err, &we) {
		t.Fatalf("err = %v (%T), want *WebhookError %s", err, err, reason)
	}
	if we.Reason != reason {
		t.Fatalf("Reason = %q, want %q", we.Reason, reason)
	}
}

func TestVerifyWebhookNilHeader(t *testing.T) {
	d := newTestDelivery(testWebhookBody)
	_, err := verifyWebhookAt(time.Unix(testWebhookNow, 0), d.body, nil, []string{d.secret})
	wantWebhookReason(t, err, "missing_header")
}

func TestVerifyWebhookCanonicalAndRawHeaderKeys(t *testing.T) {
	d := newTestDelivery(testWebhookBody)
	if _, err := d.verify(); err != nil {
		t.Fatalf("raw lower-case keys: %v", err)
	}
	canonical := http.Header{}
	for k, vs := range d.header {
		canonical.Set(k, vs[0]) // Header.Set stores Webhook-Id and friends
	}
	if _, ok := canonical["Webhook-Id"]; !ok {
		t.Fatalf("Header.Set did not canonicalize: %v", canonical)
	}
	d.header = canonical
	if _, err := d.verify(); err != nil {
		t.Fatalf("canonical keys: %v", err)
	}
}

func TestVerifyWebhookPublicClock(t *testing.T) {
	key, secret := testWebhookKey(9, 32)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	body := []byte(testWebhookBody)
	h := http.Header{}
	h.Set("webhook-id", testWebhookID)
	h.Set("webhook-timestamp", ts)
	h.Set("webhook-signature", testWebhookSign(key, testWebhookID, ts, body))
	event, err := VerifyWebhook(body, h, []string{secret})
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if event.ID != testWebhookID || event.Type != EventJobSucceeded || event.JobID != "job_go_1" ||
		event.Status != JobStatusSucceeded {
		t.Fatalf("event = %+v", event)
	}
}

func TestVerifyWebhookHeaderShapes(t *testing.T) {
	body := []byte(testWebhookBody)
	ts := strconv.FormatInt(testWebhookNow, 10)
	keyA, secretA := testWebhookKey(11, 32)
	keyB, secretB := testWebhookKey(41, 24)
	sigA := testWebhookSign(keyA, testWebhookID, ts, body)
	sigB := testWebhookSign(keyB, testWebhookID, ts, body)
	at := time.Unix(testWebhookNow, 0)

	t.Run("one key holds two signature values", func(t *testing.T) {
		h := http.Header{"webhook-id": {testWebhookID}, "webhook-timestamp": {ts}, "webhook-signature": {sigA, sigB}}
		for _, secret := range []string{secretA, secretB} {
			if _, err := verifyWebhookAt(at, body, h, []string{secret}); err != nil {
				t.Errorf("secret %s...: %v", secret[:8], err)
			}
		}
	})
	t.Run("identical webhook-id values collapse", func(t *testing.T) {
		h := http.Header{"webhook-id": {testWebhookID, testWebhookID}, "webhook-timestamp": {ts}, "webhook-signature": {sigA}}
		if _, err := verifyWebhookAt(at, body, h, []string{secretA}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("two different webhook-id values", func(t *testing.T) {
		h := http.Header{"webhook-id": {testWebhookID, testWebhookID + "x"}, "webhook-timestamp": {ts}, "webhook-signature": {sigA}}
		_, err := verifyWebhookAt(at, body, h, []string{secretA})
		wantWebhookReason(t, err, "missing_header")
	})
	t.Run("two different webhook-timestamp values", func(t *testing.T) {
		h := http.Header{"webhook-id": {testWebhookID}, "webhook-timestamp": {ts, "1795000001"}, "webhook-signature": {sigA}}
		_, err := verifyWebhookAt(at, body, h, []string{secretA})
		wantWebhookReason(t, err, "missing_header")
	})
	t.Run("an empty webhook-id value is dropped", func(t *testing.T) {
		h := http.Header{"webhook-id": {"", testWebhookID}, "webhook-timestamp": {ts}, "webhook-signature": {sigA}}
		if _, err := verifyWebhookAt(at, body, h, []string{secretA}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("only empty values", func(t *testing.T) {
		h := http.Header{"webhook-id": {"", ""}, "webhook-timestamp": {ts}, "webhook-signature": {sigA}}
		_, err := verifyWebhookAt(at, body, h, []string{secretA})
		wantWebhookReason(t, err, "missing_header")
	})
	t.Run("an empty value list", func(t *testing.T) {
		h := http.Header{"webhook-id": {}, "webhook-timestamp": {ts}, "webhook-signature": {sigA}}
		_, err := verifyWebhookAt(at, body, h, []string{secretA})
		wantWebhookReason(t, err, "missing_header")
	})
	t.Run("Unicode-folding look-alike names never match", func(t *testing.T) {
		// strings.EqualFold folds U+212A to k and U+017F to s; the helper must not.
		if !strings.EqualFold("webhoo\u212a-id", "webhook-id") {
			t.Skip("this Go folds no look-alikes")
		}
		for _, h := range []http.Header{
			{"webhoo\u212a-id": {testWebhookID}, "webhook-timestamp": {ts}, "webhook-signature": {sigA}},
			{"webhook-id": {testWebhookID}, "webhook-timestamp": {ts}, "webhook-\u017fignature": {sigA}},
		} {
			_, err := verifyWebhookAt(at, body, h, []string{secretA})
			wantWebhookReason(t, err, "missing_header")
		}
	})
}

func TestWithWebhookTolerance(t *testing.T) {
	d := newTestDelivery(testWebhookBody)
	at := func(skew int64) (WebhookEvent, error) {
		return verifyWebhookAt(time.Unix(testWebhookNow+skew, 0), d.body, d.header, []string{d.secret})
	}
	atWith := func(skew int64, opts ...WebhookOption) error {
		_, err := verifyWebhookAt(time.Unix(testWebhookNow+skew, 0), d.body, d.header, []string{d.secret}, opts...)
		return err
	}

	t.Run("zero or negative is a validation error", func(t *testing.T) {
		for _, tol := range []time.Duration{0, -time.Second, -1, time.Duration(-1 << 63)} {
			err := atWith(0, WithWebhookTolerance(tol))
			if !errors.Is(err, ErrInvalidWebhookTolerance) {
				t.Errorf("tolerance %v: err = %v, want ErrInvalidWebhookTolerance", tol, err)
			}
			var we *WebhookError
			if errors.As(err, &we) {
				t.Errorf("tolerance %v: err is a *WebhookError", tol)
			}
			if errors.Is(err, ErrInvalidWebhookSecret) {
				t.Errorf("tolerance %v: err matches ErrInvalidWebhookSecret", tol)
			}
		}
	})
	t.Run("checked before the secrets", func(t *testing.T) {
		_, err := verifyWebhookAt(time.Unix(testWebhookNow, 0), d.body, nil, nil, WithWebhookTolerance(0))
		if !errors.Is(err, ErrInvalidWebhookTolerance) {
			t.Fatalf("err = %v, want ErrInvalidWebhookTolerance", err)
		}
	})
	t.Run("the last option wins", func(t *testing.T) {
		if err := atWith(0, WithWebhookTolerance(0), WithWebhookTolerance(time.Minute)); err != nil {
			t.Errorf("0 then 1m: %v", err)
		}
		if err := atWith(0, WithWebhookTolerance(time.Minute), WithWebhookTolerance(0)); !errors.Is(err, ErrInvalidWebhookTolerance) {
			t.Errorf("1m then 0: err = %v, want ErrInvalidWebhookTolerance", err)
		}
		wantWebhookReason(t, atWith(100, WithWebhookTolerance(time.Hour), WithWebhookTolerance(10*time.Second)), "timestamp_too_old")
		if err := atWith(100, WithWebhookTolerance(10*time.Second), WithWebhookTolerance(time.Hour)); err != nil {
			t.Errorf("10s then 1h at skew 100: %v", err)
		}
	})
	t.Run("default 300 s, symmetric and inclusive", func(t *testing.T) {
		for _, skew := range []int64{-300, 0, 300} {
			if _, err := at(skew); err != nil {
				t.Errorf("skew %d: %v", skew, err)
			}
		}
		_, err := at(301)
		wantWebhookReason(t, err, "timestamp_too_old")
		_, err = at(-301)
		wantWebhookReason(t, err, "timestamp_too_new")
	})
	t.Run("500ms is accepted", func(t *testing.T) {
		if err := atWith(0, WithWebhookTolerance(500*time.Millisecond)); err != nil {
			t.Errorf("skew 0: %v", err)
		}
		wantWebhookReason(t, atWith(1, WithWebhookTolerance(500*time.Millisecond)), "timestamp_too_old")
		wantWebhookReason(t, atWith(-1, WithWebhookTolerance(500*time.Millisecond)), "timestamp_too_new")
	})
	t.Run("the largest Duration does not overflow", func(t *testing.T) {
		largest := time.Duration(1<<63 - 1)
		if err := atWith(-testWebhookNow, WithWebhookTolerance(largest)); err != nil {
			t.Errorf("max tolerance at the epoch: %v", err)
		}
	})
}

func TestVerifyWebhookPayload(t *testing.T) {
	t.Run("exact keys: ID is not id", func(t *testing.T) {
		body := strings.Replace(testWebhookBody, `"id":`, `"ID":`, 1)
		_, err := newTestDelivery(body).verify()
		wantWebhookReason(t, err, "invalid_payload")
	})
	t.Run("exact keys: Data is not data", func(t *testing.T) {
		body := strings.Replace(testWebhookBody, `"data":`, `"Data":`, 1)
		_, err := newTestDelivery(body).verify()
		wantWebhookReason(t, err, "invalid_payload")
	})
	t.Run("invalid UTF-8 with a valid signature", func(t *testing.T) {
		body := strings.Replace(testWebhookBody, "job_go_1", "job_go_\xff", 1)
		_, err := newTestDelivery(body).verify()
		wantWebhookReason(t, err, "invalid_payload")
	})
	t.Run("a second JSON value after the object", func(t *testing.T) {
		for _, tail := range []string{"}", "]", " {}", "\n1"} {
			_, err := newTestDelivery(testWebhookBody + tail).verify()
			wantWebhookReason(t, err, "invalid_payload")
		}
	})
	t.Run("surrounding JSON whitespace is fine", func(t *testing.T) {
		if _, err := newTestDelivery(" \n" + testWebhookBody + "\r\n\t").verify(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("Data numbers are json.Number", func(t *testing.T) {
		event, err := newTestDelivery(testWebhookBody).verify()
		if err != nil {
			t.Fatal(err)
		}
		if event.Data["pages"] != json.Number("3") || event.Data["ratio"] != json.Number("1.5") {
			t.Fatalf("Data = %#v, want json.Number values", event.Data)
		}
		if event.Timestamp.Location() != time.UTC || event.Timestamp.Nanosecond() != 678_000_000 {
			t.Fatalf("Timestamp = %v", event.Timestamp)
		}
	})
	t.Run("a year-0000 timestamp is invalid_payload", func(t *testing.T) {
		for _, stamp := range []string{"0000-01-01T00:00:00Z", "0000-12-31T23:59:59.999Z"} {
			body := strings.Replace(testWebhookBody, "2026-11-17T03:04:05.678Z", stamp, 1)
			_, err := newTestDelivery(body).verify()
			wantWebhookReason(t, err, "invalid_payload")
		}
	})
	t.Run("a year-0001 timestamp is fine", func(t *testing.T) {
		body := strings.Replace(testWebhookBody, "2026-11-17T03:04:05.678Z", "0001-01-01T00:00:00Z", 1)
		event, err := newTestDelivery(body).verify()
		if err != nil {
			t.Fatal(err)
		}
		if event.Timestamp.Year() != 1 {
			t.Fatalf("Timestamp = %v", event.Timestamp)
		}
	})
	t.Run("webhook.test has an empty non-nil Data", func(t *testing.T) {
		body := `{"id":"evt_t","type":"webhook.test","timestamp":"2026-11-19T00:00:00.000Z","data":{}}`
		event, err := newTestDelivery(body).verify()
		if err != nil {
			t.Fatal(err)
		}
		if event.Type != EventWebhookTest || event.Data == nil || len(event.Data) != 0 || event.JobID != "" || event.Status != "" {
			t.Fatalf("event = %+v", event)
		}
	})
}

func TestVerifyWebhookSecretValidation(t *testing.T) {
	d := newTestDelivery(testWebhookBody)
	_, short := testWebhookKey(1, 23)
	_, long := testWebhookKey(1, 65)
	cases := map[string][]string{
		"nil list":             nil,
		"empty list":           {},
		"empty secret":         {""},
		"no prefix":            {strings.TrimPrefix(d.secret, "whsec_")},
		"unpadded":             {strings.TrimRight(d.secret, "=")},
		"23 bytes":             {short},
		"65 bytes":             {long},
		"second secret is bad": {d.secret, d.secret + "x"},
		"NBSP is not trimmed":  {"\u00a0" + d.secret},
	}
	for name, secrets := range cases {
		_, err := verifyWebhookAt(time.Unix(testWebhookNow, 0), d.body, d.header, secrets)
		if !errors.Is(err, ErrInvalidWebhookSecret) {
			t.Errorf("%s: err = %v, want ErrInvalidWebhookSecret", name, err)
			continue
		}
		var we *WebhookError
		if errors.As(err, &we) || errors.Is(err, ErrInvalidWebhookTolerance) {
			t.Errorf("%s: err = %v matches the wrong error", name, err)
		}
		if len(secrets) > 0 && !strings.Contains(err.Error(), fmt.Sprintf("webhook secret %d: ", len(secrets))) {
			t.Errorf("%s: %q does not name the position", name, err)
		}
		for _, s := range secrets {
			b64 := s
			if i := strings.Index(s, "whsec_"); i >= 0 {
				b64 = s[i+len("whsec_"):]
			}
			for _, w := range windows(b64, 6) {
				if strings.Contains(err.Error(), w) {
					t.Errorf("%s: the message carries a piece of the secret", name)
					break
				}
			}
		}
	}
	// Exactly space, tab, CR and LF are trimmed.
	if _, err := verifyWebhookAt(time.Unix(testWebhookNow, 0), d.body, d.header, []string{" \t\r\n" + d.secret + "\n\r\t "}); err != nil {
		t.Errorf("trimmed secret: %v", err)
	}
}

// TestVerifyWebhookNoEcho: no error text, for any reason or validation
// failure, contains the secret text or the delivery's signature text.
func TestVerifyWebhookNoEcho(t *testing.T) {
	d := newTestDelivery(testWebhookBody)
	sig := d.sig
	at := time.Unix(testWebhookNow, 0)
	type attempt struct {
		name    string
		now     time.Time
		body    []byte
		header  http.Header
		secrets []string
	}
	withHeader := func(name string, values ...string) http.Header {
		h := http.Header{}
		for k, vs := range d.header {
			h[k] = vs
		}
		h[name] = values
		return h
	}
	_, other := testWebhookKey(77, 32)
	attempts := []attempt{
		{"missing_header", at, d.body, http.Header{"webhook-signature": {sig}}, []string{d.secret}},
		{"invalid_timestamp", at, d.body, withHeader("webhook-timestamp", "x"+sig[:4]), []string{d.secret}},
		{"invalid_signature_header", at, d.body, withHeader("webhook-signature", sig+"x"), []string{d.secret}},
		{"signature_mismatch", at, d.body, d.header, []string{other}},
		{"timestamp_too_old", at.Add(time.Hour), d.body, d.header, []string{d.secret}},
		{"timestamp_too_new", at.Add(-time.Hour), d.body, d.header, []string{d.secret}},
		{"invalid_payload", at, []byte("not json"), withHeader("webhook-signature", testWebhookSign(d.key, testWebhookID, strconv.FormatInt(testWebhookNow, 10), []byte("not json"))), []string{d.secret}},
		{"bad secret", at, d.body, d.header, []string{d.secret[:len(d.secret)-2]}},
		{"tolerance", at, d.body, d.header, []string{d.secret}},
	}
	for _, a := range attempts {
		var opts []WebhookOption
		if a.name == "tolerance" {
			opts = append(opts, WithWebhookTolerance(-1))
		}
		_, err := verifyWebhookAt(a.now, a.body, a.header, a.secrets, opts...)
		if err == nil {
			t.Fatalf("%s: err = nil", a.name)
		}
		msg := err.Error()
		var we *WebhookError
		if errors.As(err, &we) && we.Reason != a.name {
			t.Errorf("%s: Reason = %q", a.name, we.Reason)
		}
		if !strings.HasPrefix(msg, "docql: ") {
			t.Errorf("%s: %q has no docql: prefix", a.name, msg)
		}
		texts := []string{d.secret, strings.TrimPrefix(d.secret, "whsec_"), other, sig, strings.TrimPrefix(sig, "v1,")}
		for _, text := range texts {
			for _, w := range windows(text, 6) {
				if strings.Contains(msg, w) && !strings.Contains("whsec_", w) {
					t.Errorf("%s: %q carries a piece of a secret or signature", a.name, msg)
					break
				}
			}
		}
	}
}

func TestWebhookErrorText(t *testing.T) {
	for reason, want := range map[string]string{
		"signature_mismatch": "docql: webhook signature_mismatch: no v1 signature matches any secret (wrong secret or a changed body)",
		"some_future_reason": "docql: webhook some_future_reason: webhook verification failed",
	} {
		if got := (&WebhookError{Reason: reason}).Error(); got != want {
			t.Errorf("Error() = %q, want %q", got, want)
		}
	}
	if len(webhookMessages) != 7 {
		t.Fatalf("%d reasons, want 7", len(webhookMessages))
	}
}

// TestVerifyWebhookMutationsOnlyGiveEventOrWebhookError: request data (the
// body and the header names and values) can only produce a WebhookEvent or a
// *WebhookError (T-45.1-27). A seeded run mutates a valid delivery 2,000
// times; half the body mutations are re-signed so the body parser sees them.
func TestVerifyWebhookMutationsOnlyGiveEventOrWebhookError(t *testing.T) {
	rng := rand.New(rand.NewPCG(451, 7))
	base := newTestDelivery(testWebhookBody)
	// Raw lower-case keys, held in variables: the canonicalizing Header
	// methods would hide exactly the spellings this run is about.
	names := []string{"webhook-id", "webhook-timestamp", "webhook-signature"}
	idName, tsName, sigName := names[0], names[1], names[2]
	lookalikes := []string{"webhoo\u212a-id", "webhook-\u017fignature", "webhook-t\u0130mestamp", "\uff57ebhook-id", "webhook_id", "webhook-id ", "WEBHOOK-ID"}
	junk := []string{"", " ", "\x00", "\xff\xfe", "\u00e9", "v1,", "v1, ", "v2,abc", "1795000000", "+1795000000",
		"\u0661\u0662\u0663", "\r\n", "v1,====", "\t", "999999999999", "9999999999999", "-1", "0"}
	reasons := map[string]int{}
	events := 0
	randString := func() string {
		b := make([]byte, rng.IntN(48))
		for i := range b {
			b[i] = byte(rng.IntN(256))
		}
		return string(b)
	}
	for i := 0; i < 2000; i++ {
		body := append([]byte(nil), base.body...)
		header := http.Header{}
		for k, vs := range base.header {
			header[k] = append([]string(nil), vs...)
		}
		resign, nilHeader := false, false
		for n := 1 + rng.IntN(3); n > 0; n-- {
			switch rng.IntN(12) {
			case 0: // flip a body byte
				if len(body) > 0 {
					body[rng.IntN(len(body))] ^= byte(1 + rng.IntN(255))
				}
				resign = rng.IntN(2) == 0
			case 1: // truncate the body
				body = body[:rng.IntN(len(body)+1)]
				resign = rng.IntN(2) == 0
			case 2: // trailing data
				body = append(body, randString()...)
				resign = rng.IntN(2) == 0
			case 3: // a random body
				body = []byte(randString())
				resign = rng.IntN(2) == 0
			case 4: // splice a JSON fragment into the body
				frags := []string{`null`, `[]`, `{}`, `"x"`, `1e999`, `NaN`, `"\ud800"`, `{"id":1}`, `,`, `"data":null`, `"timestamp":"2026-02-30T00:00:00Z"`}
				p := rng.IntN(len(body) + 1)
				body = append(body[:p:p], append([]byte(frags[rng.IntN(len(frags))]), body[p:]...)...)
				resign = rng.IntN(2) == 0
			case 5: // drop a header
				delete(header, names[rng.IntN(len(names))])
			case 6: // add a second value
				name := names[rng.IntN(len(names))]
				header[name] = append(header[name], junk[rng.IntN(len(junk))])
			case 7: // a case variant or a look-alike name
				name := names[rng.IntN(len(names))]
				vs := header[name]
				delete(header, name)
				if rng.IntN(2) == 0 {
					header[lookalikes[rng.IntN(len(lookalikes))]] = vs
				} else {
					b := []byte(name)
					for j := range b {
						if rng.IntN(2) == 0 && b[j] >= 'a' && b[j] <= 'z' {
							b[j] -= 'a' - 'A'
						}
					}
					header[string(b)] = vs
				}
			case 8: // a junk or random value
				name := names[rng.IntN(len(names))]
				if rng.IntN(2) == 0 {
					header[name] = []string{junk[rng.IntN(len(junk))]}
				} else {
					header[name] = []string{randString()}
				}
			case 9: // nil or empty value lists, or a nil header
				switch rng.IntN(3) {
				case 0:
					header[names[rng.IntN(len(names))]] = nil
				case 1:
					header[names[rng.IntN(len(names))]] = []string{}
				default:
					nilHeader = true
				}
			case 10: // signature entry games
				{
					sig := base.sig
					forms := []string{sig + " " + sig, "v1," + sig, strings.ToUpper(sig), sig[:len(sig)-1], sig + "=",
						strings.ReplaceAll(sig, "+", "-"), sig + "\t" + sig, " " + sig + " ", "v1," + strings.Repeat("A", rng.IntN(90))}
					header[sigName] = []string{forms[rng.IntN(len(forms))]}
				}
			case 11: // a timestamp of random digits
				{
					b := make([]byte, rng.IntN(20))
					for j := range b {
						b[j] = byte('0' + rng.IntN(10))
					}
					header[tsName] = []string{string(b)}
					resign = true
				}
			}
		}
		if resign && len(header[idName]) == 1 && len(header[tsName]) == 1 {
			header[sigName] = []string{testWebhookSign(base.key, header[idName][0], header[tsName][0], body)}
		}

		if nilHeader {
			header = nil
		}
		event, err := func() (ev WebhookEvent, err error) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("mutation %d panicked: %v", i, r)
				}
			}()
			return verifyWebhookAt(time.Unix(testWebhookNow, 0), body, header, []string{base.secret})
		}()
		if err == nil {
			events++
			if event.Data == nil || event.ID == "" || event.Type == "" || event.Timestamp.Location() != time.UTC {
				t.Fatalf("mutation %d: incomplete event %+v", i, event)
			}
			continue
		}
		we, ok := err.(*WebhookError)
		if !ok {
			t.Fatalf("mutation %d: err = %v (%T), want only *WebhookError", i, err, err)
		}
		if _, known := webhookMessages[we.Reason]; !known {
			t.Fatalf("mutation %d: unknown reason %q", i, we.Reason)
		}
		reasons[we.Reason]++
	}
	// The run must reach every stage, the body parser included.
	for reason := range webhookMessages {
		if reason == "timestamp_too_new" || reason == "timestamp_too_old" {
			continue // only the re-signed random timestamps reach the window
		}
		if reasons[reason] == 0 {
			t.Errorf("no mutation gave %s (reasons %v, events %d)", reason, reasons, events)
		}
	}
	if reasons["timestamp_too_old"]+reasons["timestamp_too_new"] == 0 {
		t.Errorf("no mutation reached the tolerance window (reasons %v)", reasons)
	}
	t.Logf("2000 mutations: %d events, reasons %v", events, reasons)
}
