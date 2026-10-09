package docql

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/textproto"
	"strings"
)

// newBoundary returns a fresh random multipart boundary: 16 bytes from
// crypto/rand, hex-encoded to 32 lower-case characters. Tests replace this
// var to pin the golden boundary.
var newBoundary = func() string {
	b := make([]byte, 16)
	// crypto/rand.Read never fails and always fills b entirely (Go 1.24+).
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// escapeFilename escapes exactly the three characters that would break the
// quoted filename parameter of the file part (D-02): " becomes %22, CR
// becomes %0D and LF becomes %0A. There is no filename* form.
func escapeFilename(name string) string {
	return strings.NewReplacer(`"`, "%22", "\r", "%0D", "\n", "%0A").Replace(name)
}

// paramsBody is the params object of the body part: present only when a mode
// was given (D-03 of the wire contract).
type paramsBody struct {
	Mode Mode `json:"mode"`
}

// bodyDoc is the body part: query or prompt first, then params. The pointer
// fields keep Query("") emitting "query":"" while absent fields are omitted.
type bodyDoc struct {
	Query  *string     `json:"query,omitempty"`
	Prompt *string     `json:"prompt,omitempty"`
	Params *paramsBody `json:"params,omitempty"`
}

// bodyJSON encodes the body part: compact canonical UTF-8 with no HTML
// escaping, and the one trailing newline the Encoder appends is trimmed
// (D-03 of the wire contract). A zero Instruction is rejected here too, so
// no caller can frame a body with neither field.
func bodyJSON(instr Instruction, mode *Mode) ([]byte, error) {
	doc := bodyDoc{}
	switch instr.kind {
	case instructionQuery:
		q := instr.text
		doc.Query = &q
	case instructionPrompt:
		p := instr.text
		doc.Prompt = &p
	default:
		return nil, ErrNoInstruction
	}
	if mode != nil {
		doc.Params = &paramsBody{Mode: *mode}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte{'\n'}), nil
}

// frame builds the (preamble, epilogue) multipart framing around the file
// bytes with mime/multipart CreatePart (locked Phase 10 wire decision): the
// file part carries a hand-built header, so the filename keeps the %22/%0D/%0A
// escaping and application/octet-stream is always the part content type (D-01
// of the wire contract). The form-file helper of mime/multipart is never used:
// it backslash-escapes the filename, which the server rejects. The body part
// carries the canonical JSON. Everything is CRLF-terminated, byte-exact with
// the golden fixtures.
func frame(filename string, body []byte, boundary string) (preamble, epilogue []byte, err error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.SetBoundary(boundary); err != nil {
		return nil, nil, err
	}

	fileHeader := textproto.MIMEHeader{}
	fileHeader.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name="file"; filename="%s"`, escapeFilename(filename)))
	fileHeader.Set("Content-Type", "application/octet-stream")
	if _, err := mw.CreatePart(fileHeader); err != nil {
		return nil, nil, err
	}
	preamble = bytes.Clone(buf.Bytes())
	buf.Reset()

	bodyHeader := textproto.MIMEHeader{}
	bodyHeader.Set("Content-Disposition", `form-data; name="body"`)
	part, err := mw.CreatePart(bodyHeader)
	if err != nil {
		return nil, nil, err
	}
	if _, err := part.Write(body); err != nil {
		return nil, nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, nil, err
	}
	return preamble, bytes.Clone(buf.Bytes()), nil
}

// upload is a framed multipart upload ready to send (D-07). contentLength is
// the exact byte count body yields when the file length is known, so the
// request carries an exact Content-Length and never chunked framing; -1 means
// unknown. fileSize is the measured file length excluding framing, -1 when
// unknown. body is always a multi-reader over preamble, file and epilogue; a
// pipe is never used (locked GO-03 wire rule). getBody returns a fresh,
// identical reader, so the request can be replayed.
type upload struct {
	contentType   string
	contentLength int64 // -1 = unknown
	fileSize      int64 // -1 = unknown
	body          io.ReadCloser
	getBody       func() (io.ReadCloser, error)
}

// prepareUpload validates the call and frames the upload for the bytes path
// (D-06, D-07). The part name follows the strict filename rule: WithFilename
// wins verbatim, else the name carried by the File; empty is
// ErrMissingFilename — there is no default filename. Later File kinds
// (paths, readers) return ErrNoFile until their plans land.
func prepareUpload(file File, instr Instruction, qs querySettings) (*upload, error) {
	if file.kind != fileBytes {
		return nil, ErrNoFile
	}
	partName := file.name
	if qs.filename != nil {
		partName = *qs.filename
	}
	if partName == "" {
		return nil, ErrMissingFilename
	}

	body, err := bodyJSON(instr, qs.mode)
	if err != nil {
		return nil, err
	}
	boundary := newBoundary()
	preamble, epilogue, err := frame(partName, body, boundary)
	if err != nil {
		return nil, err
	}

	buildBody := func() (io.ReadCloser, error) {
		return io.NopCloser(io.MultiReader(
			bytes.NewReader(preamble),
			bytes.NewReader(file.data),
			bytes.NewReader(epilogue),
		)), nil
	}
	first, err := buildBody()
	if err != nil {
		return nil, err
	}
	return &upload{
		contentType:   "multipart/form-data; boundary=" + boundary,
		contentLength: int64(len(preamble) + len(file.data) + len(epilogue)),
		fileSize:      int64(len(file.data)),
		body:          first,
		getBody:       buildBody,
	}, nil
}
