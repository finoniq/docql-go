# DocQL Go SDK

A Go client for the DocQL document-extraction API (`POST /v1/query-document`).
Module path: `github.com/finoniq/docql-go`, package `docql`. The module is
standard library only, so adding it cannot add a single dependency to yours.

## Install

```sh
go get github.com/finoniq/docql-go@v0.1.0
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

`QueryDocument` takes one `docql.File`, built with `docql.FilePath(path)`,
`docql.FileBytes(data, name)` or `docql.FileReader(r, name)`:

- The filename rule: `docql.WithFilename` is sent verbatim; otherwise a path
  uses its base name; otherwise the `name` argument of the constructor is
  used. There is no default filename — when the final name would be empty,
  the call fails with `docql.ErrMissingFilename` before anything is sent.
- A reader is read from its current position and never closed. A file the SDK
  opened from a path is closed by the SDK.
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

The SDK never retries — every call is billed. Retry yourself, only when
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
version is 0, a minor release may change the API; pin `v0.1.0` until 1.0. A
bad release is never re-tagged — it is withdrawn with a `retract` directive
in a higher version.

## Support

When asking for help, quote the request id — `res.RequestID` on success, the
`RequestID` field of a `*docql.Error` on failure. Never share your API key.
Report security issues through [SECURITY.md](SECURITY.md).

## License

Apache-2.0 — see [LICENSE](LICENSE).
