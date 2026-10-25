package docql

import "time"

const (
	// defaultAPIURL is used when neither WithAPIURL nor DOCQL_API_URL yields
	// a value (D-01).
	defaultAPIURL = "https://api-docql.finoniq.com"
	// envAPIKey and envAPIURL are the environment variables NewClient falls
	// back to (D-01).
	envAPIKey = "DOCQL_API_KEY"
	envAPIURL = "DOCQL_API_URL"
	// queryPath is appended to the API URL for every call.
	queryPath = "/v1/query-document"
)

const (
	// defaultTimeout is the overall per-call deadline (D-02): it covers the
	// upload, the server wait and the response read. A caller deadline that
	// is earlier wins.
	defaultTimeout = 660 * time.Second
	// connectTimeout bounds dial and TLS handshake on the transport the SDK
	// builds itself (D-02); an injected client keeps its own transport.
	connectTimeout = 10 * time.Second
)

const (
	// errorBodyKeepChars is how much of a response body is kept on Error.Body
	// (D-09), counted in runes.
	errorBodyKeepChars = 2048
	// errorBodyReadLimit bounds how much of a non-2xx body is read from the
	// wire, so a misbehaving proxy cannot exhaust memory.
	errorBodyReadLimit = 65536
	// maxUploadBytes is the API's upload ceiling; over it, connection errors
	// carry a size hint, because an early 413 can be lost as a reset (D-10).
	maxUploadBytes = 26214400
	// snippetMaxChars bounds the body snippet in the non-DocQL form of
	// Error.Error (D-09).
	snippetMaxChars = 200
)

// The DocQL reasons (D-08, D-09): exactly the 22 non-null reasons of the
// vendored contract kit, contract/sdk-cases.json (the sync stub and gateway
// errors) plus contract/job-cases.json (the job errors). Error.Reason carries
// these values; classification is by status, never by body shape. Adding
// constants later is backward compatible, removing them is not.
const (
	// ReasonInvalidAPIKey is the 401 reason for a missing or unknown key.
	ReasonInvalidAPIKey = "invalid_api_key"
	// ReasonUploadTooLarge is the 413 reason for an upload over the limit.
	ReasonUploadTooLarge = "upload_too_large"
	// ReasonUnsupportedMediaType is the 415 reason for a non-multipart request.
	ReasonUnsupportedMediaType = "unsupported_media_type"
	// ReasonMissingFilePart is the 422 reason for a request without its file part.
	ReasonMissingFilePart = "missing_file_part"
	// ReasonMissingBodyField is the 422 reason for a request without its body field.
	ReasonMissingBodyField = "missing_body_field"
	// ReasonBodyNotJSON is the 422 reason for a body field that is not JSON.
	ReasonBodyNotJSON = "body_not_json"
	// ReasonMissingQueryOrPrompt is the 422 reason for neither query nor prompt.
	ReasonMissingQueryOrPrompt = "missing_query_or_prompt"
	// ReasonInvalidParams is the 422 reason for invalid params.
	ReasonInvalidParams = "invalid_params"
	// ReasonUnsupportedFileType is the 422 reason for an unsupported file type.
	ReasonUnsupportedFileType = "unsupported_file_type"
	// ReasonCorruptedFile is the 422 reason for a file that cannot be parsed.
	ReasonCorruptedFile = "corrupted_file"
	// ReasonDocumentExceedsTimeBudget is the 422 reason for a document over the time budget.
	ReasonDocumentExceedsTimeBudget = "document_exceeds_time_budget"
	// ReasonServiceOverloaded is the 429 reason telling the caller to retry later.
	ReasonServiceOverloaded = "service_overloaded"
	// ReasonUpstreamUnavailable is the 503 reason for an unavailable upstream.
	ReasonUpstreamUnavailable = "upstream_unavailable"
	// ReasonUpstreamAuthFailed is the 502 reason for a gateway-to-upstream auth failure.
	ReasonUpstreamAuthFailed = "upstream_auth_failed"
	// ReasonConcurrencyLimitExceeded is the gateway 429 reason for an account
	// with too many requests in progress.
	ReasonConcurrencyLimitExceeded = "concurrency_limit_exceeded"
	// ReasonRateLimitExceeded is the gateway 429 reason for an account over its
	// requests-per-minute limit.
	ReasonRateLimitExceeded = "rate_limit_exceeded"
	// ReasonJobNotFound is the 404 reason for an unknown job id.
	ReasonJobNotFound = "job_not_found"
	// ReasonJobExpired is the 404 reason for a job whose result has expired.
	ReasonJobExpired = "job_expired"
	// ReasonQueueFull is the 429 reason for a job queue with no free slot.
	ReasonQueueFull = "queue_full"
	// ReasonIdempotencyKeyConflict is the 422 reason for an idempotency key
	// reused with a different request.
	ReasonIdempotencyKeyConflict = "idempotency_key_conflict"
	// ReasonIdempotencyInProgress is the 409 reason for an idempotency key whose
	// first request is still being processed.
	ReasonIdempotencyInProgress = "idempotency_in_progress"
	// ReasonJobInterrupted is the 503 reason carried only inside a failed job's
	// error, for a job interrupted before it could complete.
	ReasonJobInterrupted = "job_interrupted"
)

// The job routes and the wait pacing (JSDK-01). The wait numbers copy the
// contract kit's reference waitForJob (D-03 as matched to the kit, D-04,
// OD-5), so every SDK sleeps the same seconds on the same answers.
const (
	// jobsPath is the submit route; one job lives at jobsPath + "/" + id.
	jobsPath = "/v1/jobs"
	// pollTimeout bounds each GET of the wait, GetJob and CancelJob, never
	// the 660 s upload deadline (26-RESEARCH Pitfall 3). Each poll's deadline
	// is also capped by the wait's own deadline.
	pollTimeout = 30 * time.Second
	// defaultWaitTimeout is the wait's deadline when the caller's context
	// has none (OD-5).
	defaultWaitTimeout = 15 * time.Minute
	// retryAfterFloor and retryAfterCap clamp every Retry-After the wait
	// honours (D-03).
	retryAfterFloor = 1 * time.Second
	retryAfterCap   = 30 * time.Second
	// retryAfterAbsent is the sleep after a non-terminal 200 without
	// Retry-After: the kit's value (26-RESEARCH F6).
	retryAfterAbsent = 1 * time.Second
	// transientBackoffCap caps the 1, 2, 4, 8, 16 s backoff of a transient
	// failure without Retry-After (D-04).
	transientBackoffCap = 30 * time.Second
)
