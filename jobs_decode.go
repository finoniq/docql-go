package docql

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// parseJob decodes a 2xx job answer (submit 202, poll 200, cancel 200 or 202)
// into *Job (D-05, D-09). A body that is not exactly one JSON object with a
// string job_id and a string status is unusable and returns *Error the way
// parseResult reports an unusable 2xx. A succeeded job's result decodes
// through parseResult, so it is typed like QueryDocument's answer; a failed
// job's embedded error decodes through decodeError with the embedded
// status_code (500 when it is not a JSON integer), then JobID is set and
// Retryable forced false: a failed job is final, and resubmitting it with
// the same key replays the same failure. idemKey is the key a SubmitDocument
// sent; "" for GetJob, Wait and CancelJob, which cannot know it.
func parseJob(status int, h http.Header, body []byte, apiKey, idemKey string) (*Job, error) {
	unusable := func(message string) (*Job, error) {
		return nil, &Error{
			Status:    status,
			RequestID: sanitizeServerString(h.Get("X-Request-Id"), apiKey, 0),
			Message:   message,
			Body:      scrubAndTruncate(body, apiKey),
		}
	}

	// Exactly one JSON object, then EOF, as parseResult requires.
	var top map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&top); err != nil || top == nil {
		return unusable("unexpected non-JSON response")
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return unusable("unexpected non-JSON response")
	}

	var jobID, jobStatus *string
	if json.Unmarshal(top["job_id"], &jobID) != nil || jobID == nil ||
		json.Unmarshal(top["status"], &jobStatus) != nil || jobStatus == nil {
		return unusable("unexpected response: not a DocQL job object")
	}

	metadata := jsonObject(top["metadata"])

	job := &Job{
		JobID:          *jobID,
		Status:         JobStatus(*jobStatus),
		PageCount:      jsonIntPtr(metadata["page_count"]),
		PagesDone:      jsonIntPtr(metadata["pages_done"]),
		RequestID:      sanitizeServerString(h.Get("X-Request-Id"), apiKey, 0),
		CreatedAt:      jsonTime(top["created_at"]),
		StartedAt:      jsonTime(top["started_at"]),
		FinishedAt:     jsonTime(top["finished_at"]),
		ExpiresAt:      jsonTime(top["expires_at"]),
		RetryAfter:     parseRetryAfter(h.Get("Retry-After"), now()),
		IdempotencyKey: idemKey,
	}

	if raw, ok := top["result"]; ok && job.Status == JobStatusSucceeded && isJSONObject(raw) {
		res, err := parseResult(http.StatusOK, h, raw, apiKey)
		if err != nil {
			return nil, err
		}
		job.Result = res
	}

	if raw, ok := top["error"]; ok && job.Status == JobStatusFailed && isJSONObject(raw) {
		code := http.StatusInternalServerError
		if n := jsonIntPtr(jsonObject(raw)["status_code"]); n != nil {
			code = *n
		}
		e := decodeError(code, http.Header{}, raw, apiKey)
		e.JobID = job.JobID
		e.Retryable = false
		job.Error = e
	}
	return job, nil
}

// jsonObject decodes a raw JSON value into a map with numbers kept as
// json.Number; anything that is not an object gives an empty map.
func jsonObject(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var v any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if d.Decode(&v) != nil {
		return map[string]any{}
	}
	m, ok := v.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return m
}

// isJSONObject reports whether a raw JSON value is an object.
func isJSONObject(raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(raw, &m) == nil && m != nil
}

// jsonTime parses an RFC 3339 timestamp string; absent, null, a non-string
// or an unparsable value gives nil.
func jsonTime(raw json.RawMessage) *time.Time {
	var s *string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil || s == nil {
		return nil
	}
	t, err := time.Parse(time.RFC3339, *s)
	if err != nil {
		return nil
	}
	return &t
}
