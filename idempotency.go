package docql

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
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
// The cap counts SDK-level attempts only. net/http itself treats a POST that
// carries an Idempotency-Key header and a non-nil GetBody as replayable, and
// re-sends it once when a reused keep-alive connection fails before any
// response byte arrives (26-RESEARCH F9, Pitfall 7). That hidden attempt
// carries the same key and the same bytes, so DocQL answers it as a replay
// and it can never create a second job. GetBody stays set on purpose: the
// v0.1 upload relies on it, and clearing it would only trade that safe
// replay for a spurious connection error.
func (c *Client) submitWithRetry(ctx context.Context, up *upload, key string) (*Job, error) {
	replayable := up.getBody != nil
	for attempt := 1; ; attempt++ {
		send := up
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
			again := *up
			again.body = b
			send = &again
		}

		status, h, body, err := c.sendOnce(ctx, http.MethodPost, jobsPath, send, []string{key}, c.cfg.timeout)
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
