package docql_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"

	docql "github.com/finoniq/docql-go"
)

// ExampleClient_QueryDocument is the README quickstart: it reads
// DOCQL_API_KEY and DOCQL_API_URL, uploads invoice.pdf and prints the request
// id, the page count and the extraction data. scripts/check-example.sh fails
// when this body and the README's main() drift apart. It has no Output
// comment, so `go test` compiles it without running it.
func ExampleClient_QueryDocument() {
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

// ExampleClient_SubmitDocument is the README jobs example: it submits
// invoice.pdf as a job, waits until the job has finished and prints its
// status and, for a succeeded job, the extraction data. scripts/check-example.sh
// fails when this body and the README's main() drift apart. It has no Output
// comment, so `go test` compiles it without running it.
func ExampleClient_SubmitDocument() {
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

// ExampleClient_QueryDocument_spreadsheet is the README spreadsheet example:
// it sends sales.csv as bytes with an explicit .csv file name and prints the
// page count; when the server refuses the sheet as too large, it reads the
// axis, its value and the limit from Error.Metadata. scripts/check-example.sh
// fails when this body and the README's main() drift apart. It has no Output
// comment, so `go test` compiles it without running it.
func ExampleClient_QueryDocument_spreadsheet() {
	client, err := docql.NewClient() // reads DOCQL_API_KEY and DOCQL_API_URL
	if err != nil {
		log.Fatal(err)
	}

	data, err := os.ReadFile("sales.csv")
	if err != nil {
		log.Fatal(err)
	}

	res, err := client.QueryDocument(
		context.Background(),
		docql.FileBytes(data, ""),
		docql.Query("{ region total_sales }"),
		docql.WithFilename("sales.csv"), // a CSV is recognised by its .csv name
	)
	var apiErr *docql.Error
	if errors.As(err, &apiErr) && apiErr.Reason == docql.ReasonSpreadsheetTooLarge {
		limit, _ := apiErr.Metadata["limit"].(json.Number)
		for _, axis := range []string{"cells", "runes", "estimated_output_tokens", "bytes"} {
			if value, ok := apiErr.Metadata[axis].(json.Number); ok {
				fmt.Printf("too large: %s=%s limit=%s\n", axis, value, limit)
				return
			}
		}
	}
	if err != nil {
		log.Fatal(err)
	}

	if res.PageCount != nil {
		fmt.Println("pages:", *res.PageCount)
	}
}

// ExampleVerifyWebhook is the README webhook receiver: it reads the raw body
// of each POST (at most 1 MiB), verifies it with DOCQL_WEBHOOK_SECRET,
// answers 400 for a refused delivery and 204 after printing the event type
// and id. scripts/check-example.sh fails when this body and the README's
// main() drift apart. It has no Output comment, so `go test` compiles it
// without running it.
func ExampleVerifyWebhook() {
	secret := os.Getenv("DOCQL_WEBHOOK_SECRET") // whsec_..., from the console
	port := os.Getenv("PORT")
	if port == "" {
		port = "8000"
	}

	http.HandleFunc("POST /", func(w http.ResponseWriter, r *http.Request) {
		// The raw bytes, before any JSON decoding; at most 1 MiB.
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				w.WriteHeader(http.StatusRequestEntityTooLarge)
			} else {
				w.WriteHeader(http.StatusBadRequest)
			}
			return
		}

		event, err := docql.VerifyWebhook(body, r.Header, []string{secret})
		var whErr *docql.WebhookError
		if errors.As(err, &whErr) {
			log.Printf("webhook refused: %s", whErr.Reason)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if err != nil { // a missing or malformed secret: fix the configuration
			log.Print(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		fmt.Printf("event %s %s\n", event.Type, event.ID) // store event.ID; skip one already seen
		w.WriteHeader(http.StatusNoContent)
	})

	log.Fatal(http.ListenAndServe("127.0.0.1:"+port, nil))
}
