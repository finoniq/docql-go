package docql

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// QueryResult is a successful answer of QueryDocument (D-11). Unknown
// metadata fields reach callers through Metadata, so new engine fields surface
// without an SDK release.
type QueryResult struct {
	// Data is the envelope's data object as raw JSON; absent or null data is
	// {}.
	Data json.RawMessage
	// RequestID comes from metadata, else the X-Request-Id header; "" when
	// neither is present.
	RequestID string
	// PageCount is nil when the engine omits it or it is not a JSON integer
	// (D-11): absent and 0 stay distinct, which billing in pages needs.
	PageCount *int
	// PagesOCR is nil under the same rule as PageCount.
	PagesOCR *int
	// Metadata is the full raw metadata, never nil.
	Metadata map[string]any
}

// parseResult decodes a 2xx body into *QueryResult (D-11). A body that is not
// a JSON object, or whose data is present, non-null and not an object, is
// unusable: it returns *Error naming the problem instead of an empty result.
// The key scrubs the Body stored on that error, symmetric with decodeError.
func parseResult(status int, h http.Header, body []byte, key string) (*QueryResult, error) {
	unusable := func(message string) (*QueryResult, error) {
		return nil, &Error{
			Status:    status,
			RequestID: h.Get("X-Request-Id"),
			Message:   message,
			Body:      scrubAndTruncate(body, key),
		}
	}

	var top map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&top); err != nil || top == nil || dec.More() {
		return unusable("unexpected non-JSON response")
	}

	var data json.RawMessage
	if raw, ok := top["data"]; ok && !isNullJSON(raw) {
		var v any
		vd := json.NewDecoder(bytes.NewReader(raw))
		vd.UseNumber()
		if err := vd.Decode(&v); err != nil {
			return unusable("unexpected response: data is not a JSON object")
		}
		if _, isObj := v.(map[string]any); !isObj {
			return unusable("unexpected response: data is not a JSON object")
		}
		data = raw
	}
	if data == nil {
		data = json.RawMessage("{}")
	}

	metadata := map[string]any{}
	if raw, ok := top["metadata"]; ok && !isNullJSON(raw) {
		var v any
		md := json.NewDecoder(bytes.NewReader(raw))
		md.UseNumber()
		if err := md.Decode(&v); err == nil {
			if m, isObj := v.(map[string]any); isObj {
				metadata = m
			}
		}
	}

	requestID := ""
	if rid, ok := metadata["request_id"].(string); ok {
		requestID = rid
	} else {
		requestID = h.Get("X-Request-Id")
	}

	return &QueryResult{
		Data:      data,
		RequestID: requestID,
		PageCount: jsonIntPtr(metadata["page_count"]),
		PagesOCR:  jsonIntPtr(metadata["pages_ocr"]),
		Metadata:  metadata,
	}, nil
}

// isNullJSON reports whether a raw JSON value is the null literal.
func isNullJSON(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// jsonIntPtr converts a decoded JSON value to *int only when it is a
// json.Number whose literal is a plain integer that fits an int (D-11):
// floats, booleans and strings stay nil, matching the exact-integer rule of
// the reference client.
func jsonIntPtr(v any) *int {
	n, ok := v.(json.Number)
	if !ok || strings.ContainsAny(n.String(), ".eE") {
		return nil
	}
	i, err := strconv.Atoi(n.String())
	if err != nil {
		return nil
	}
	return &i
}
