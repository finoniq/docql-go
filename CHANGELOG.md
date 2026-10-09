# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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

[0.1.0]: https://github.com/finoniq/docql-go/tree/v0.1.0
