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
