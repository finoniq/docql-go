package docql_test

import (
	"context"
	"fmt"
	"log"

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
