package docql

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// JobStatus is the status of a job (D-09). It is an open string: compare it
// with the JobStatus constants, and expect a newer DocQL to answer a status
// this SDK does not know, which is kept verbatim.
type JobStatus string

// The job statuses DocQL answers today.
const (
	// JobStatusQueued: the job waits for a worker.
	JobStatusQueued JobStatus = "queued"
	// JobStatusRunning: a worker is extracting the document.
	JobStatusRunning JobStatus = "running"
	// JobStatusSucceeded: the job is done and Job.Result holds the answer.
	JobStatusSucceeded JobStatus = "succeeded"
	// JobStatusFailed: the job failed and Job.Error holds the reason.
	JobStatusFailed JobStatus = "failed"
	// JobStatusCancelled: the job was cancelled before it finished.
	JobStatusCancelled JobStatus = "cancelled"
)

// Job is one DocQL job as the API answered it (JSDK-01).
type Job struct {
	// JobID is the opaque job id; pass it to GetJob, Wait and CancelJob.
	JobID string
	// Status is the job status; an unknown value is kept verbatim (D-09).
	Status JobStatus
	// PageCount is the document's page count; nil while DocQL does not know
	// it or when it is not a JSON integer, so absent and 0 stay distinct.
	PageCount *int
	// PagesDone is the number of pages extracted so far, under the same nil
	// rule as PageCount.
	PagesDone *int
	// RequestID is the X-Request-Id of the answer that produced this Job;
	// "" when absent.
	RequestID string
	// CreatedAt, StartedAt, FinishedAt and ExpiresAt are the job's
	// timestamps; nil when null, absent or not an RFC 3339 time.
	CreatedAt  *time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time
	ExpiresAt  *time.Time
	// Result is the extraction result of a succeeded job, typed like
	// QueryDocument's answer; nil otherwise.
	Result *QueryResult
	// Error is the error of a failed job, with JobID set and Retryable
	// false; nil otherwise. GetJob returns a failed job with a nil error.
	Error *Error
	// RetryAfter is the answer's Retry-After in whole seconds, 0 when
	// absent, for callers who poll GetJob themselves.
	RetryAfter time.Duration
	// IdempotencyKey is the key SubmitDocument sent; "" on a Job from
	// GetJob, Wait or CancelJob, which cannot know it.
	IdempotencyKey string
}

// jobPath is the URL path of one job: the id as a single percent-encoded
// segment with every "." as %2E, the gateway's own encoder, so "." and ".."
// ids are never normalised away. The id is opaque: only "" is refused.
func jobPath(id string) (string, error) {
	if id == "" {
		return "", ErrNoJobID
	}
	return jobsPath + "/" + strings.ReplaceAll(url.PathEscape(id), ".", "%2E"), nil
}

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

// sendOnce sends exactly one job request and returns the status, headers and
// body (QueryDocument's request block, copied rather than shared so the sync
// path stays as it was). Every request carries X-API-Key and User-Agent; an
// upload adds Content-Type, the exact Content-Length when known, GetBody and
// one Idempotency-Key line per given key. No X-Request-Id or x-docql-*
// header is ever set. timeout bounds the whole call, under ctx. Redirects
// are refused by the client's CheckRedirect, so a 3xx comes back as a
// status. It never decodes and never retries; keys are validated by the
// caller. A transport failure is a *ConnectionError carrying the key when
// exactly one was sent.
func (c *Client) sendOnce(ctx context.Context, method, path string, up *upload, keys []string, timeout time.Duration) (int, http.Header, []byte, error) {
	ctx2, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	oneKey := ""
	if len(keys) == 1 {
		oneKey = keys[0]
	}
	fail := func(err error) (int, http.Header, []byte, error) {
		ce := c.jobConnectionError(ctx2, err, up, timeout)
		ce.IdempotencyKey = oneKey
		return 0, nil, nil, ce
	}

	var body io.Reader
	if up != nil {
		body = up.body
	}
	req, err := http.NewRequestWithContext(ctx2, method, c.cfg.apiURL+path, body)
	if err != nil {
		return fail(err)
	}
	req.Header.Set("X-API-Key", c.cfg.apiKey)
	req.Header.Set("User-Agent", userAgent())
	if up != nil {
		req.Header.Set("Content-Type", up.contentType)
		req.ContentLength = up.contentLength
		req.GetBody = up.getBody
	}
	for _, k := range keys {
		req.Header.Add("Idempotency-Key", k)
	}

	resp, err := c.cfg.httpClient.Do(req)
	if err != nil {
		return fail(err)
	}
	defer resp.Body.Close()

	var data []byte
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		data, err = io.ReadAll(resp.Body)
	} else {
		data, err = io.ReadAll(io.LimitReader(resp.Body, errorBodyReadLimit))
	}
	if err != nil {
		return fail(err)
	}
	return resp.StatusCode, resp.Header, data, nil
}

// jobConnectionError is connectionError for job requests: the same mapping,
// with the timeout the job request actually ran under in the message.
func (c *Client) jobConnectionError(ctx context.Context, cause error, up *upload, timeout time.Duration) *ConnectionError {
	if errors.Is(cause, errShortRead) {
		message := fmt.Sprintf("docql: the file changed size during upload: %v", cause)
		return &ConnectionError{msg: scrub(message, c.cfg.apiKey), cause: cause}
	}
	if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(cause, ctxErr) {
		cause = fmt.Errorf("%w: %w", cause, ctxErr)
	}
	isTimeout := ctx.Err() == context.DeadlineExceeded
	var ne net.Error
	if errors.As(cause, &ne) && ne.Timeout() {
		isTimeout = true
	}
	message := fmt.Sprintf("docql: connection to %s failed: %v", c.cfg.apiURL, cause)
	if isTimeout {
		message = fmt.Sprintf("docql: request timed out after %s: %v", timeout, cause)
	}
	if up != nil && up.fileSize > maxUploadBytes {
		message += fmt.Sprintf(" (the upload is %d bytes; the API rejects files over 25 MiB)", up.fileSize)
	}
	return &ConnectionError{msg: scrub(message, c.cfg.apiKey), cause: cause, timeout: isTimeout}
}

// SubmitDocument uploads one document with an instruction as an async job
// and returns the queued Job (JSDK-01). It takes the QueryDocument options
// plus WithIdempotencyKey; without that option it sends a fresh random key.
// Job.IdempotencyKey, Error.IdempotencyKey and ConnectionError.IdempotencyKey
// carry the key sent, so a failed submit can be retried safely with
// WithIdempotencyKey(key): DocQL answers the same job instead of creating a
// second one. The per-call deadline is the client's WithTimeout value, as for
// QueryDocument. Every client-side check happens before a byte is sent.
func (c *Client) SubmitDocument(ctx context.Context, file File, instr Instruction, opts ...QueryOption) (*Job, error) {
	if c == nil || c.cfg == nil {
		return nil, ErrNilClient
	}
	if ctx == nil {
		return nil, ErrNilContext
	}
	if instr.kind == instructionNone {
		return nil, ErrNoInstruction
	}
	if file.kind == fileNone {
		return nil, ErrNoFile
	}
	var qs querySettings
	for _, opt := range opts {
		opt(&qs)
	}
	key := ""
	if qs.idempotencyKey != nil {
		key = *qs.idempotencyKey
		if err := validateIdempotencyKey(key); err != nil {
			return nil, err
		}
	} else {
		key = newIdempotencyKey()
	}
	up, err := prepareUpload(file, instr, qs)
	if err != nil {
		return nil, err
	}
	// A file the SDK opened from a path is closed on every path (D-07).
	defer up.closer.close()

	status, h, body, err := c.sendOnce(ctx, http.MethodPost, jobsPath, up, []string{key}, c.cfg.timeout)
	if err != nil {
		return nil, err
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
	return nil, e
}

// jobCall checks the arguments every job-id method shares and returns the
// job's URL path.
func (c *Client) jobCall(ctx context.Context, jobID string) (string, error) {
	if c == nil || c.cfg == nil {
		return "", ErrNilClient
	}
	if ctx == nil {
		return "", ErrNilContext
	}
	return jobPath(jobID)
}

// jobAnswer turns one job answer into a Job or an *Error carrying jobID.
func (c *Client) jobAnswer(status int, h http.Header, body []byte, jobID string) (*Job, error) {
	if status >= 200 && status < 300 {
		job, err := parseJob(status, h, body, c.cfg.apiKey, "")
		if err != nil {
			withJobID(err, jobID)
			return nil, err
		}
		return job, nil
	}
	e := decodeError(status, h, body, c.cfg.apiKey)
	e.JobID = jobID
	return nil, e
}

// withJobID sets JobID on an *Error that does not carry one yet.
func withJobID(err error, jobID string) {
	var e *Error
	if errors.As(err, &e) && e.JobID == "" {
		e.JobID = jobID
	}
}

// GetJob fetches the job once (T-E2): any 2xx is a Job, a failed job
// included (its Error is set and the returned error is nil); any other
// status is an *Error with JobID set. A 404 with Reason "" means the
// endpoint does not serve jobs. Each call is one GET with a 30 s deadline
// under ctx; nothing retries.
func (c *Client) GetJob(ctx context.Context, jobID string) (*Job, error) {
	path, err := c.jobCall(ctx, jobID)
	if err != nil {
		return nil, err
	}
	status, h, body, err := c.sendOnce(ctx, http.MethodGet, path, nil, nil, pollTimeout)
	if err != nil {
		return nil, err
	}
	return c.jobAnswer(status, h, body, jobID)
}

// Wait polls the job until it is succeeded or cancelled and returns it
// (JSDK-01, D-03 to D-06). It is the contract kit's reference wait:
//
//   - each sleep is the answer's Retry-After clamped to [1, 30] s, 1 s after
//     a queued or running answer without one; WithPollInterval raises every
//     sleep to at least its value; there is no jitter;
//   - a network error, a per-poll timeout, a 429 and a 503 are retried, after
//     their Retry-After or else a 1, 2, 4, 8, 16, 30 s backoff that resets
//     after any 200;
//   - a failed job returns its *Error (JobID set, Retryable false); a 401,
//     any 404 and any other status return their *Error, with JobID set, at
//     once; a status this SDK does not know is polled like queued (D-09).
//
// The deadline is ctx's, else 15 minutes. When the next sleep would pass it,
// Wait blocks until it passes; when ctx is cancelled or the deadline passes,
// Wait returns a *WaitError for which errors.Is(err, ctx.Err()) holds. Each
// poll is one GET with a 30 s deadline under the wait's. Wait never cancels
// the job on any path; only CancelJob does.
func (c *Client) Wait(ctx context.Context, jobID string, opts ...WaitOption) (*Job, error) {
	path, err := c.jobCall(ctx, jobID)
	if err != nil {
		return nil, err
	}
	var ws waitSettings
	for _, opt := range opts {
		opt(&ws)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultWaitTimeout)
		defer cancel()
	}
	deadline, _ := ctx.Deadline()
	stopped := func() (*Job, error) {
		return nil, &WaitError{JobID: jobID, err: ctx.Err()}
	}

	backoff := retryAfterFloor
	for {
		var hint time.Duration
		hasHint, transient := false, false
		status, h, body, err := c.sendOnce(ctx, http.MethodGet, path, nil, nil, pollTimeout)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return stopped()
			}
			transient = true // a network error or a per-poll timeout
		case status >= 200 && status < 300:
			job, perr := c.jobAnswer(status, h, body, jobID)
			if perr != nil {
				return nil, perr
			}
			switch job.Status {
			case JobStatusSucceeded, JobStatusCancelled:
				return job, nil
			case JobStatusFailed:
				if job.Error != nil {
					return nil, job.Error
				}
				return nil, &Error{
					Status:    status,
					RequestID: job.RequestID,
					Message:   "unexpected response: failed job without an error object",
					Body:      scrubAndTruncate(body, c.cfg.apiKey),
					JobID:     jobID,
				}
			}
			backoff = retryAfterFloor // queued, running or unknown (D-09)
			hint, hasHint = retryAfterHint(h)
		case status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable:
			transient = true
			hint, hasHint = retryAfterHint(h)
		default:
			_, e := c.jobAnswer(status, h, body, jobID)
			return nil, e
		}

		var d time.Duration
		switch {
		case hasHint:
			d = min(max(hint, retryAfterFloor), retryAfterCap)
		case transient:
			d = backoff
			backoff = min(backoff*2, transientBackoffCap)
		default:
			d = retryAfterAbsent
		}
		d = max(d, ws.pollInterval)

		if d > time.Until(deadline) {
			// Sleeping would pass the deadline: block until it passes, so
			// the error wraps ctx.Err() (26-RESEARCH Pitfall 4).
			<-ctx.Done()
			return stopped()
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return stopped()
		case <-t.C:
		}
		if ctx.Err() != nil {
			return stopped()
		}
		// The timer and the deadline can fire at the same instant, before
		// the context's own timer has cancelled it: never poll at or past
		// the deadline (26-RESEARCH Pitfall 4).
		if !time.Now().Before(deadline) {
			<-ctx.Done()
			return stopped()
		}
	}
}

// retryAfterHint returns the Retry-After of a job answer and whether one was
// present and parsable. Unlike Error.RetryAfter, it tells "Retry-After: 0"
// (clamped to 1 s) from an absent header (the 1 s default or the backoff).
func retryAfterHint(h http.Header) (time.Duration, bool) {
	text := strings.TrimSpace(h.Get("Retry-After"))
	if isASCIIDigits(text) {
		d := parseRetryAfter(text, now())
		if d == 0 && strings.Trim(text, "0") != "" {
			return retryAfterCap, true // too large for a Duration: clamped anyway
		}
		return d, true
	}
	if _, err := http.ParseTime(text); err == nil {
		return parseRetryAfter(text, now()), true
	}
	return 0, false
}

// CancelJob asks DocQL to cancel the job, with one DELETE under a 30 s
// deadline, and returns the Job as the answer states it (OD-10): a 202
// answers the job right after the cancel request, a 200 a job that was
// already terminal (cancelling it again answers the same job). Poll with
// GetJob or Wait to confirm the job reached cancelled. Any other status is an
// *Error with JobID set. This is the only call that cancels a job.
func (c *Client) CancelJob(ctx context.Context, jobID string) (*Job, error) {
	path, err := c.jobCall(ctx, jobID)
	if err != nil {
		return nil, err
	}
	status, h, body, err := c.sendOnce(ctx, http.MethodDelete, path, nil, nil, pollTimeout)
	if err != nil {
		return nil, err
	}
	return c.jobAnswer(status, h, body, jobID)
}
