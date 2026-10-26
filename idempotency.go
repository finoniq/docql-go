package docql

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// newIdempotencyKey returns a fresh Idempotency-Key from crypto/rand (D-07).
func newIdempotencyKey() string {
	return rand.Text()
}

// validateIdempotencyKey applies DocQL's rule: 1-255 bytes, every byte
// 0x20-0x7E. The error never echoes the key.
func validateIdempotencyKey(k string) error {
	if len(k) < 1 || len(k) > 255 {
		return ErrInvalidIdempotencyKey
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x20 || k[i] > 0x7E {
			return ErrInvalidIdempotencyKey
		}
	}
	return nil
}

// submitWithRetry sends the submit with one Idempotency-Key and a bounded
// same-key retry (JSDK-02, D-07, D-08), the loop docql-python's
// submit_with_retry and docql-java's submitDocument also run.
//
// DocQL's handoff (Revision 2026-10-21) says a submit 503 may already have
// created the job: "treat every submit 503 as 'a job may exist' and retry
// with the same Idempotency-Key". So, with key on every attempt and at most
// submitMaxAttempts attempts:
//
//   - any 503, and a 409 idempotency_in_progress (another submit with this
//     key is still being persisted), is retried when the upload is
//     replayable, that is when up.getBody is non-nil (FileBytes, FilePath, a
//     FileReader over a working io.Seeker); each sleep is Retry-After clamped
//     to [1, 30] s, else 1, 2, 4 s (submitBackoff), on a fresh time.NewTimer
//     that a cancelled ctx stops;
//   - a 429 (queue_full, rate_limit_exceeded, concurrency_limit_exceeded: no
//     job exists), every other status, a transport error and a timeout are
//     returned at once;
//   - the key is never regenerated for a retry (a new key could create a
//     second billable job), and an upload without getBody (a plain
//     io.Reader, a *bytes.Buffer, a pipe) is sent exactly once.
//
// Every *Error and *ConnectionError returned here carries IdempotencyKey, so
// the caller can retry safely with WithIdempotencyKey.
//
// rewindsCaller is true when the retry re-reads the caller's own reader (a
// FileReader over an io.Seeker). Only then does a retry wait, before it
// re-reads the input, until the transport has closed every body of the
// previous attempt (attemptBodies). net/http's RoundTripper contract lets the
// transport keep reading, and close, a request body after RoundTrip has
// returned an early answer: a 503 sent before the upload was consumed leaves
// the write loop inside the caller's Read, and rewinding the caller's
// io.Seeker under it would corrupt the retry's bytes. The wait ends when that
// body is closed or when ctx ends, which returns the same *ConnectionError as
// a ctx end during the retry sleep. FileBytes and FilePath retries do not
// wait: their getBody builds an independent reader (fresh bytes.Readers over
// the same immutable data, or a newly opened file), so the previous attempt's
// transport can keep its body as long as it likes (26-REVIEW WR-03: an
// injected RoundTripper that never closes the body must not block the call).
//
// The cap counts SDK-level attempts only. net/http itself treats a POST that
// carries an Idempotency-Key header and a non-nil GetBody as replayable, and
// re-sends it once when a reused keep-alive connection fails before any
// response byte arrives (26-RESEARCH F9, Pitfall 7). That hidden attempt
// carries the same key and the same bytes, so DocQL answers it as a replay
// and it can never create a second job. GetBody stays set on purpose: the
// v0.1 upload relies on it, and clearing it would only trade that safe
// replay for a spurious connection error.
func (c *Client) submitWithRetry(ctx context.Context, up *upload, key string, rewindsCaller bool) (*Job, error) {
	replayable := up.getBody != nil
	for attempt := 1; ; attempt++ {
		var reqBody io.ReadCloser = up.body
		if attempt > 1 {
			// A fresh, identical body from the start of the input; the
			// boundary, length and content type of attempt 1 still hold.
			b, err := up.getBody()
			if err != nil {
				return nil, &ConnectionError{
					IdempotencyKey: key,
					msg:            scrub(fmt.Sprintf("docql: could not re-read the file for the same-key retry: %v", err), c.cfg.apiKey),
					cause:          err,
				}
			}
			reqBody = b
		}
		bodies := &attemptBodies{}
		send := *up
		send.body = bodies.track(reqBody)
		if up.getBody != nil {
			send.getBody = func() (io.ReadCloser, error) {
				b, err := up.getBody()
				if err != nil {
					return nil, err
				}
				return bodies.track(b), nil
			}
		}

		status, h, body, err := c.sendOnce(ctx, http.MethodPost, jobsPath, &send, []string{key}, c.cfg.timeout)
		if err != nil {
			return nil, err // a *ConnectionError that already carries the key
		}
		if status >= 200 && status < 300 {
			job, perr := parseJob(status, h, body, c.cfg.apiKey, key)
			if perr != nil {
				var e *Error
				if errors.As(perr, &e) {
					e.IdempotencyKey = key
				}
				return nil, perr
			}
			return job, nil
		}
		e := decodeError(status, h, body, c.cfg.apiKey)
		e.IdempotencyKey = key
		retryable := status == http.StatusServiceUnavailable ||
			(status == http.StatusConflict && e.Reason == ReasonIdempotencyInProgress)
		if !retryable || !replayable || attempt >= submitMaxAttempts {
			return nil, e
		}

		d := submitBackoff[attempt-1]
		if hint, ok := retryAfterHint(h); ok {
			d = min(max(hint, retryAfterFloor), retryAfterCap)
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, submitStopped(ctx, status, key)
		case <-t.C:
		}
		// The next getBody rewinds the caller's reader: wait until the
		// transport has released this attempt's body (see above).
		if rewindsCaller {
			select {
			case <-bodies.released():
			case <-ctx.Done():
				return nil, submitStopped(ctx, status, key)
			}
		}
		// The timer and a ctx deadline can fire at the same instant: never
		// send once ctx has ended.
		if ctx.Err() != nil {
			return nil, submitStopped(ctx, status, key)
		}
	}
}

// submitStopped is the error of a submit whose ctx ended during a retry
// sleep: a *ConnectionError that carries the key and wraps ctx.Err(), so
// errors.Is(err, context.Canceled) or context.DeadlineExceeded holds.
func submitStopped(ctx context.Context, status int, key string) *ConnectionError {
	cause := ctx.Err()
	return &ConnectionError{
		IdempotencyKey: key,
		msg:            fmt.Sprintf("docql: submit stopped while waiting to retry after HTTP %d: %v", status, cause),
		cause:          cause,
		timeout:        errors.Is(cause, context.DeadlineExceeded),
	}
}

// attemptBodies tracks the request bodies one submit attempt hands to the
// transport: its first body and any GetBody replay net/http makes on its own.
// released reports when every one of them has been closed, that is when the
// transport has finished with the input (the RoundTripper contract: "callers
// wanting to reuse the body for subsequent requests must arrange to wait for
// the Close call before doing so").
type attemptBodies struct {
	mu   sync.Mutex
	open int
	idle chan struct{} // set while released waits; closed when open drops to 0
}

// track wraps one body handed to the transport.
func (a *attemptBodies) track(b io.ReadCloser) io.ReadCloser {
	a.mu.Lock()
	a.open++
	a.mu.Unlock()
	return &trackedBody{ReadCloser: b, owner: a}
}

// released returns a channel that is closed once every tracked body has been
// closed. It is called after the attempt's request has returned, when
// net/http hands out no more bodies for it.
func (a *attemptBodies) released() <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	ch := make(chan struct{})
	if a.open == 0 {
		close(ch)
	} else {
		a.idle = ch
	}
	return ch
}

func (a *attemptBodies) closed() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.open--
	if a.open == 0 && a.idle != nil {
		close(a.idle)
		a.idle = nil
	}
}

// trackedBody is a request body whose first Close tells its attemptBodies.
type trackedBody struct {
	io.ReadCloser
	owner *attemptBodies
	once  sync.Once
}

func (b *trackedBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.owner.closed)
	return err
}
