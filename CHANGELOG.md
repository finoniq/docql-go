# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.4.0] - 2026-11-24

### Added

- `VerifyWebhook(body, header, secrets, opts...)` verifies a webhook
  delivery signed under the open Standard Webhooks spec and returns a
  `WebhookEvent` with `ID`, `Type`, `Timestamp` (UTC), `JobID`, `Status` and
  `Data` (numbers as `json.Number`, never nil). It needs no client and no API
  key. Pass several secrets during a rotation: a delivery signed with any of
  them verifies.
- `WithWebhookTolerance(d)`, the clock difference allowed either way
  (default 5 minutes).
- `ErrInvalidWebhookSecret` and `ErrInvalidWebhookTolerance` (match with
  `errors.Is`) for a missing or malformed secret and for a tolerance of zero
  or less.
- `*WebhookError` (match with `errors.As`) for a refused delivery. Its
  `Reason` is one of `missing_header`, `invalid_timestamp`,
  `invalid_signature_header`, `signature_mismatch`, `timestamp_too_old`,
  `timestamp_too_new` or `invalid_payload`.
- `EventJobSucceeded`, `EventJobFailed`, `EventJobCancelled` and
  `EventWebhookTest`.
- README § Webhooks and the pkg.go.dev example `ExampleVerifyWebhook`: a
  standard-library receiver, run against a delivery signed at run time.

### Changed

- Contract kit v6 is vendored. Its success bodies no longer carry
  `metadata.screenshot`, and the SDK decodes a success with or without it.
- The install line and § Versioning name v0.4.

### Correction to 0.3.0

The 0.3.0 entry said there were no breaking changes. In 0.3.0 `Error` gained
the `Metadata map[string]any` field, so `docql.Error` is no longer comparable
with `==` and cannot be a map key. Code that compares `*Error` pointers or
fields, or uses `errors.As`, is unaffected. The 0.3.0 entry below now says
so. 0.4.0 does not change `Error`.

No breaking changes: code written against v0.3.0 builds and runs unchanged.

## [0.3.0] - 2026-11-23

### Added

- `Error.Metadata`: the server's raw refusal metadata object of an error
  answer, with every number as a `json.Number` (exact for any int64) and the
  API key scrubbed from keys and string values. It is never nil: a body
  without a metadata object gives an empty map. `Error()` is unchanged.
- Three reason constants: `ReasonSpreadsheetTooLarge`,
  `ReasonDocumentTooComplex` and `ReasonTooManyPages` (25 in all).
- A README and pkg.go.dev spreadsheet example
  (`ExampleClient_QueryDocument_spreadsheet`): it sends a CSV by an explicit
  `.csv` name and reads the axis, its value and the limit of a
  `spreadsheet_too_large` refusal from `Error.Metadata`.
- README § Files states the CSV file-name rule: a CSV is recognised only by
  a name ending in `.csv` in any letter case, `FileBytes` and `FileReader`
  need the name, and the name is sent verbatim (`"a.csv "` is refused).

### Changed

- Contract kit v5 is vendored (stub pin `sha-1a3c3ce`); the contract suite
  runs all 26 spreadsheet cases on the sync and the jobs route.
- README § Errors lists all 12 validation sentinels, and the install line
  and § Versioning name v0.3.

The module path stays `github.com/finoniq/docql-go` and `go.mod` still says
`go 1.26` with no dependency.

Corrected 2026-11-24: this entry first said there were no breaking changes.
That was wrong. `Error` gained the `Metadata map[string]any` field, so
`docql.Error` is no longer comparable with `==` and cannot be a map key.
Code that compares `*Error` pointers or fields, or uses `errors.As`, is
unaffected.

## [0.2.1] - 2026-10-26

### Fixed

- WR-03: `SubmitDocument` no longer blocks forever when an injected
  `http.RoundTripper` never closes the request body after a retryable 503.
  `FileBytes` and `FilePath` retries do not wait (each retry reads an
  independent copy), and a `FileReader` retry waits at most the smaller of
  the client timeout and 30 s, then returns the 503/409 `*Error` with
  `IdempotencyKey` instead of rewinding a reader that may still be in use.

No breaking changes: code written against v0.2.0 builds and runs unchanged.

## [0.2.0] - 2026-10-26

### Added

- Async jobs for long documents: `SubmitDocument(ctx, file, instr, opts...)`,
  `GetJob(ctx, jobID)`, `Wait(ctx, jobID, opts...)` and
  `CancelJob(ctx, jobID)`.
- `Job` with `JobID`, `Status`, `PageCount`, `PagesDone`, `RequestID`, the
  `CreatedAt` / `StartedAt` / `FinishedAt` / `ExpiresAt` timestamps, `Result`
  (a `*QueryResult` on a succeeded job), `Error` (a `*Error` on a failed job),
  `RetryAfter` and `IdempotencyKey`.
- `JobStatus` with `JobStatusQueued`, `JobStatusRunning`,
  `JobStatusSucceeded`, `JobStatusFailed` and `JobStatusCancelled`; an
  unknown status from a newer API is kept verbatim.
- Eight new reason constants: `ReasonConcurrencyLimitExceeded`,
  `ReasonRateLimitExceeded`, `ReasonJobNotFound`, `ReasonJobExpired`,
  `ReasonQueueFull`, `ReasonIdempotencyKeyConflict`,
  `ReasonIdempotencyInProgress` and `ReasonJobInterrupted`.
- `Error.JobID`, `Error.IdempotencyKey` and `ConnectionError.IdempotencyKey`;
  `*WaitError` (with `JobID`, `Timeout()` and `Unwrap()`, so
  `errors.Is(err, ctx.Err())` holds).
- The options `WithIdempotencyKey` (a `QueryOption`) and `WithPollInterval`
  (a `WaitOption`).
- The sentinel errors `ErrInvalidIdempotencyKey`, `ErrNoJobID`,
  `ErrIdempotencyKeyOnQuery` (`QueryDocument` refuses `WithIdempotencyKey`),
  `ErrInvalidAPIKey` and `ErrNilClient`.
- Every submit carries an `Idempotency-Key`, generated with `crypto/rand`
  when you pass none. A 503 or a 409 `idempotency_in_progress` on submit is
  retried with that same key, at most 4 attempts, and only for input that can
  be replayed from its start (`FilePath`, `FileBytes`, or a `FileReader` over
  an `io.Seeker`). Before a retry reads the input again, `SubmitDocument`
  waits until the HTTP transport has closed the previous attempt's body, so a
  reader is never rewound while the transport may still read it.
- `Wait` sleeps the server's `Retry-After` between polls (clamped to 1-30 s),
  retries network errors, 429 and 503, and stops with a `*WaitError` when
  your context ends or its deadline passes (15 minutes when the context has
  none). It never sends a poll at or past its deadline: a sleep that ends
  exactly on the deadline ends the wait. It never cancels the job.

### Changed

- Nothing breaking. 0.1.0's "the SDK never retries" now applies to
  `QueryDocument` only: `SubmitDocument` retries as described above, and
  `QueryDocument`, `GetJob` and `CancelJob` each send exactly one request.

### Fixed

- CR-01: `NewClient` no longer panics when `http.DefaultTransport` has been
  replaced by a type other than `*http.Transport`.
- R1-WR-01: `NewClient` trims the API key and refuses one with a control
  character inside (`ErrInvalidAPIKey`), instead of failing later as a
  connection error.
- R1-WR-02: an SDK-opened file is closed exactly once, without a data race,
  when the transport and the caller close it at the same time.
- WR-01: `Reason`, `RequestID`, `Message` and `QueryResult.RequestID` are
  cleaned of the API key and of control characters, and `Message` is cut at
  2048 characters.
- WR-02: a 2xx body with anything after its JSON object is reported as
  unusable.
- WR-03: `QueryDocument` on a nil or zero `Client` returns `ErrNilClient`
  instead of panicking.
- WR-04: the `FileReader` and `FileBytes` docs state that the input belongs to
  the call until it returns, and that the transport may still read it
  briefly after an early error answer.
- WR-05: the release workflow re-checks, after the approval gate, that the
  tag still points at the commit the precheck validated.
- WR-06: the release workflow no longer passes `secrets: inherit` to the
  contract workflow, which only needs `GITHUB_TOKEN`.
- WR-07: CI runs the unit tests on the `go.mod` floor (Go 1.26) and on the
  current stable Go, and fails if the floor cell drifts from `go.mod`.

No breaking changes: code written against v0.1.0 builds and runs unchanged.

## [0.1.0] - 2026-10-09

### Added

- `docql.NewClient(opts ...docql.Option)` with `WithAPIKey`, `WithAPIURL`,
  `WithTimeout` and `WithHTTPClient`; the `DOCQL_API_KEY` / `DOCQL_API_URL`
  environment fallbacks, the 660 s overall per-call deadline (an earlier
  deadline on the caller's context wins), and an injected `*http.Client` used
  through a shallow copy with its `Timeout` cleared and redirects refused. A
  `*docql.Client` is safe for concurrent use and has no `Close`.
- `QueryDocument(ctx, file, instr, opts...)` with the file constructors
  `FileBytes`, `FilePath` and `FileReader` (strict filename rule, no default
  filename; known-length bodies with `ContentLength` and `GetBody`; an
  unmeasurable reader sent chunked), the sealed `Query` / `Prompt`
  instructions, and the per-call options `WithMode` and `WithFilename`.
  Nothing retries — every call is billed.
- `QueryResult` with `Data`, `RequestID`, `PageCount`, `PagesOCR` and the raw
  `Metadata`; page counts are pointers, so "absent" stays distinct from 0.
- `*docql.Error` for every non-2xx answer (envelope or not) with `Status`,
  `Reason`, `RequestID`, `RetryAfter`, `Retryable`, `Message` and a
  key-scrubbed `Body`; `*docql.ConnectionError` for transport failures with
  `Timeout()` and `Unwrap()` and a size hint on uploads over 25 MiB; the
  validation sentinel errors `ErrMissingAPIKey`, `ErrInvalidAPIURL`,
  `ErrInvalidTimeout`, `ErrNilContext`, `ErrNoInstruction`, `ErrNoFile` and
  `ErrMissingFilename`. The API key never appears in any error text or in
  `String` / `GoString`.
- `Mode` with `ModeFast` / `ModeStandard`, the fourteen `Reason…` constants,
  and the exported `Version`.

[0.3.0]: https://github.com/finoniq/docql-go/tree/v0.3.0
[0.2.1]: https://github.com/finoniq/docql-go/tree/v0.2.1
[0.2.0]: https://github.com/finoniq/docql-go/tree/v0.2.0
[0.1.0]: https://github.com/finoniq/docql-go/tree/v0.1.0
