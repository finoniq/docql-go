// Package docql is a Go client for the DocQL document-extraction API.
//
// The single call is QueryDocument: it uploads one document with an
// instruction (a query or a prompt) and returns the extraction result.
//
//	import docql "github.com/finoniq/docql-go"
//
//	client, err := docql.NewClient() // reads DOCQL_API_KEY / DOCQL_API_URL
//	if err != nil {
//		return err
//	}
//	res, err := client.QueryDocument(ctx,
//		docql.FileBytes(data, "invoice.pdf"),
//		docql.Query("What is the total amount?"),
//	)
//
// NewClient takes everything as options (D-01). WithAPIKey and WithAPIURL win;
// otherwise the DOCQL_API_KEY and DOCQL_API_URL environment variables are used
// (a whitespace-only value counts as unset), and the URL additionally falls
// back to https://api-docql.finoniq.com.
//
// A *Client is safe for concurrent use by multiple goroutines and has no
// Close method (D-03): the transport it builds is never torn down by the SDK.
package docql
