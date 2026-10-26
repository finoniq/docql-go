# DocQL Go SDK

A Go client for the DocQL document-extraction API (`POST /v1/query-document`).
Module path: `github.com/finoniq/docql-go`, package `docql`. The module is
standard library only, so adding it cannot add a single dependency to yours.

## Install

```sh
go get github.com/finoniq/docql-go@v0.2.1
```

```go
import docql "github.com/finoniq/docql-go"
```

## Requirements

- Go 1.26 or newer.
- No third-party dependencies: the module uses only the Go standard library.

## Quickstart

Set `DOCQL_API_KEY`, and set `DOCQL_API_URL` to the API origin you were given — the production endpoint `https://api-docql.finoniq.com` is not live yet.

<!-- docql:quickstart:start -->
```go
package main

import (
	"context"
	"fmt"
	"log"

	docql "github.com/finoniq/docql-go"
)

func main() {
	client, err := docql.NewClient() // reads DOCQL_API_KEY and DOCQL_API_URL
	if err != nil {
		log.Fatal(err)
	}

	res, err := client.QueryDocument(
		context.Background(),
		docql.FilePath("invoice.pdf"),
		docql.Query("{ invoice_number total_amount }"),
	)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("request id:", res.RequestID)
	if res.PageCount != nil {
		fmt.Println("pages:", *res.PageCount)
	}
	fmt.Println(string(res.Data))
}
```
<!-- docql:quickstart:end -->

The same code is `ExampleClient_QueryDocument` on
[pkg.go.dev](https://pkg.go.dev/github.com/finoniq/docql-go). The query uses
DocQL's brace-and-field syntax; pass `docql.Prompt(...)` instead for a
natural-language instruction — a call takes exactly one `docql.Query` or
`docql.Prompt`.

## Long documents: jobs

Use a job for a long document: a proxied edge cuts a single call at about
100–120 s, while `docql.ModeStandard` may take up to 540 s. `SubmitDocument`
uploads the file and answers at once with a queued job, and `Wait` polls it
until it finishes.

<!-- docql:jobs:start -->
```go
package main

import (
	"context"
	"fmt"
	"log"

	docql "github.com/finoniq/docql-go"
)

func main() {
	client, err := docql.NewClient() // reads DOCQL_API_KEY and DOCQL_API_URL
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()

	job, err := client.SubmitDocument(
		ctx,
		docql.FilePath("invoice.pdf"),
		docql.Query("{ invoice_number total_amount }"),
	)
	if err != nil {
		log.Fatal(err)
	}

	job, err = client.Wait(ctx, job.JobID) // polls until succeeded or cancelled
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(job.Status)
	if job.Result != nil {
		fmt.Println(string(job.Result.Data))
	}
}
```
<!-- docql:jobs:end -->

The same code is `ExampleClient_SubmitDocument` on
[pkg.go.dev](https://pkg.go.dev/github.com/finoniq/docql-go).

- `GetJob(ctx, jobID)` makes one poll and returns the job as it is; a `failed`
  job is returned too (read `job.Error`), with a nil error.
- `CancelJob(ctx, jobID)` asks DocQL to cancel and returns the job at that
  moment, which may still be `docql.JobStatusRunning`: poll to confirm.
- `Wait` returns a `succeeded` or `cancelled` job and returns the job's
  `*docql.Error` for a `failed` one. Its deadline is your `ctx`'s, else 15
  minutes, and it never sends a poll at or past that deadline. When `ctx` is
  cancelled or the deadline passes, it returns a `*docql.WaitError` that
  carries `JobID` and for which `errors.Is(err, ctx.Err())` holds.
  `docql.WithPollInterval(d)` raises the shortest sleep between polls. `Wait`
  never cancels the job: `GetJob` or another `Wait` picks it up again.
- Every submit carries an `Idempotency-Key`, generated for you unless you pass
  `docql.WithIdempotencyKey(key)`; it is on the returned job
  (`Job.IdempotencyKey`) and on every error the submit returns
  (`Error.IdempotencyKey`, `ConnectionError.IdempotencyKey`). DocQL scopes the
  key to your organisation. Unlike `QueryDocument`, `SubmitDocument` retries a
  503 and a 409 `idempotency_in_progress` with the same key, at most 4
  attempts in all (a 503 may mean the job was created anyway), but only for
  input it can send again from its start: `docql.FilePath` (reopened),
  `docql.FileBytes`, or a `docql.FileReader` over an `io.Seeker` (rewound to
  where it was when you called `SubmitDocument`, and only once the transport
  has let go of the previous attempt's body; if it has not after the smaller
  of the client timeout and 30 s, the 503 or 409 comes back with its key
  instead of a rewind). Any other reader is sent once.
  A job that ended `failed`, `job_interrupted` included, needs a new key: the
  old one replays the failure.
- Go's transport may also re-send the submit once on its own, when a reused
  connection fails before any answer arrives. That hidden attempt carries the
  same key and the same bytes, so DocQL answers it with the same job.
- `job.PagesDone` stays 0 while DocQL runs OCR, so show progress as
  indeterminate while a running job reports 0 pages done.

## Configuration

| Option | Env variable | Default | Notes |
| --- | --- | --- | --- |
| `docql.WithAPIKey(key)` | `DOCQL_API_KEY` | none | required; `docql.NewClient` returns an error matching `docql.ErrMissingAPIKey` when neither is set |
| `docql.WithAPIURL(u)` | `DOCQL_API_URL` | `https://api-docql.finoniq.com` | trailing `/` ignored; `http://` allowed for local stacks; the default is not live yet |
| `docql.WithTimeout(d)` | — | 660 s | the overall per-call deadline: it includes the upload, so on a slow uplink a large file uses part of it (the Python SDK instead applies a per-read inactivity timeout); an earlier deadline on your `context` wins |
| `docql.WithHTTPClient(hc)` | — | none | the SDK uses a shallow copy with `Timeout` cleared and redirects refused, and never changes your client; the 10 s connect limit applies only to the SDK's own transport |

Options win over the environment variables.

**Concurrency:** a `*docql.Client` is safe for concurrent use by multiple
goroutines — create one and reuse it. There is no `Close` method; the
SDK-built transport lives as long as your process needs it.

**Local stacks behind Caddy `tls internal`:** Go uses the system trust store,
which does not know the local CA. Set `SSL_CERT_FILE` to the exported root
certificate, or inject an `*http.Client` whose transport's
`TLSClientConfig.RootCAs` trusts it:

```go
pool := x509.NewCertPool()
pool.AppendCertsFromPEM(rootPEM)
transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}
client, err := docql.NewClient(docql.WithHTTPClient(&http.Client{Transport: transport}))
```

Never turn certificate verification off.

## Files

`QueryDocument` and `SubmitDocument` take one `docql.File`, built with `docql.FilePath(path)`,
`docql.FileBytes(data, name)` or `docql.FileReader(r, name)`:

- The filename rule: `docql.WithFilename` is sent verbatim; otherwise a path
  uses its base name; otherwise the `name` argument of the constructor is
  used. There is no default filename — when the final name would be empty,
  the call fails with `docql.ErrMissingFilename` before anything is sent.
- A reader is read from its current position and never closed. A file the SDK
  opened from a path is closed by the SDK.
- A reader belongs to the call until the call returns: do not read, seek,
  reuse or mutate it (or the buffer behind it) meanwhile. After an early error
  answer, such as a `413` or `401` before the whole upload was sent, the HTTP
  transport may still read from it briefly after the call has returned, so
  retry with a fresh reader (reopen the file, or a new `bytes.Reader` over
  unchanged data) rather than rewinding the same one. Do not modify
  `FileBytes` data after passing it to a call either. `SubmitDocument`'s own
  same-key retry waits for the transport to let go of the previous attempt's
  body before it rewinds a reader, for at most the smaller of the client
  timeout and 30 s; past that it returns the 503 or 409 with its key and
  never rewinds the reader.
- There are no client-side type or size checks — the server is the authority.
- A reader whose length cannot be measured is sent chunked; an oversized
  chunked upload may answer `503` instead of `413`. The API rejects files
  over 25 MiB, and a transport failure on a larger file carries a size hint
  in its message.

## Modes

`docql.ModeFast` and `docql.ModeStandard` name the two processing modes.
`docql.Mode` is an open type: any other value passes through and the server
validates it. Omitting `docql.WithMode` means the server default.

## Errors

Three kinds:

- `*docql.Error` (match with `errors.As`) — the server answered non-2xx. It
  carries `Status`, `Reason`, `RequestID`, `RetryAfter`, `Retryable`,
  `Message` and a key-scrubbed `Body` truncated to 2048 characters.
- `*docql.ConnectionError` (match with `errors.As`) — no usable answer
  arrived (connect, TLS, reset, short read, timeout). `Timeout()` reports a
  deadline expiry, and `errors.Is` keeps working for
  `context.DeadlineExceeded` and `context.Canceled`.
- The validation sentinels (match with `errors.Is`): `docql.ErrMissingAPIKey`,
  `docql.ErrInvalidAPIURL`, `docql.ErrInvalidTimeout`, `docql.ErrNilContext`,
  `docql.ErrNoInstruction`, `docql.ErrNoFile`, `docql.ErrMissingFilename`.

Every reason below is exported as a `docql.Reason…` constant
(`invalid_api_key` is `docql.ReasonInvalidAPIKey`):

| Status | Reason | What to do |
| --- | --- | --- |
| 401 | `docql.ReasonInvalidAPIKey` (`invalid_api_key`) | the key is wrong or revoked — check `DOCQL_API_KEY` or issue a new key |
| 413 | `docql.ReasonUploadTooLarge` (`upload_too_large`) | the file exceeds the API limit (25 MiB) — shrink or split it |
| 415 | `docql.ReasonUnsupportedMediaType` (`unsupported_media_type`) | the request was not multipart/form-data — send it through `QueryDocument` |
| 422 | `docql.ReasonMissingFilePart` (`missing_file_part`) | the multipart body has no file part |
| 422 | `docql.ReasonMissingBodyField` (`missing_body_field`) | the multipart body has no `body` form field |
| 422 | `docql.ReasonBodyNotJSON` (`body_not_json`) | the `body` form field is not valid JSON |
| 422 | `docql.ReasonMissingQueryOrPrompt` (`missing_query_or_prompt`) | pass exactly one of `docql.Query` or `docql.Prompt` |
| 422 | `docql.ReasonInvalidParams` (`invalid_params`) | check the parameters — for example an unknown `mode` |
| 422 | `docql.ReasonUnsupportedFileType` (`unsupported_file_type`) | the API does not accept this file type |
| 422 | `docql.ReasonCorruptedFile` (`corrupted_file`) | the file could not be parsed |
| 422 | `docql.ReasonDocumentExceedsTimeBudget` (`document_exceeds_time_budget`) | the document is too large for the mode — try `docql.ModeStandard` or split it |
| 429 | `docql.ReasonServiceOverloaded` (`service_overloaded`) | retryable — wait `RetryAfter` seconds and retry |
| 502 | `docql.ReasonUpstreamAuthFailed` (`upstream_auth_failed`) | not retryable — quote the request id when contacting support |
| 503 | `docql.ReasonUpstreamUnavailable` (`upstream_unavailable`) | retryable — wait `RetryAfter` seconds and retry |
| any | any other reason | read `Message`; only 429 and 503 set `Retryable` |
| any | an empty `Reason` | a non-DocQL response: a framework 404/405 means a wrong `WithAPIURL`; an empty 5xx or an HTML 524 from an edge means the call may still have been processed |
| 3xx | — | the API URL redirects; the SDK never follows redirects, so set `WithAPIURL` to the final URL |

## Retrying

`QueryDocument` never retries — every call is billed. Retry it yourself, only when
`errors.As` finds a `*docql.Error` with `Retryable` true, sleeping
`RetryAfter` when it is greater than 0 and a capped exponential backoff
otherwise, with at most 5 attempts and respect for your context:

```go
func queryWithRetry(ctx context.Context, client *docql.Client, file docql.File, instr docql.Instruction) (*docql.QueryResult, error) {
	for attempt := 1; ; attempt++ {
		res, err := client.QueryDocument(ctx, file, instr)
		if err == nil {
			return res, nil
		}
		var apiErr *docql.Error
		if attempt >= 5 || !errors.As(err, &apiErr) || !apiErr.Retryable {
			return nil, err
		}
		wait := apiErr.RetryAfter
		if wait <= 0 {
			wait = time.Duration(1<<uint(attempt)) * time.Second
			if wait > 30*time.Second {
				wait = 30 * time.Second
			}
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
```

Go's transport may re-send a request only when nothing was written to a
reused connection, which is not a retry of a processed call.

## Timeouts and billing

Usage is metered in pages. A timed-out call may still have been billed — the
server may finish the work after the client gave up. For the same reason,
502/504/524 answers are not retryable: only 429 and 503 mean the request was
not processed.

## Versioning

The SDK follows [Semantic Versioning](https://semver.org/). While the major
version is 0, a minor release may change the API; pin a `v0.2.x` version
until 1.0. A bad release is never re-tagged — it is withdrawn with a
`retract` directive in a higher version.

## Support

When asking for help, quote the request id — `res.RequestID` on success, the
`RequestID` field of a `*docql.Error` on failure. Never share your API key.
Report security issues through [SECURITY.md](SECURITY.md).

## License

Apache-2.0 — see [LICENSE](LICENSE).
