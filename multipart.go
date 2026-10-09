package docql

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"mime/multipart"
	"net/textproto"
	"os"
	"path/filepath"
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

// openFile opens a path for reading (a seam tests wrap to track opens and
// closes of the files the SDK opens from a path).
var openFile = os.Open

// upload is a framed multipart upload ready to send (D-07). contentLength is
// the exact byte count body yields when the file length is known, so the
// request carries an exact Content-Length and never chunked framing; -1 means
// unknown. fileSize is the measured file length excluding framing, -1 when
// unknown. body is always a multi-reader over preamble, file and epilogue; a
// pipe is never used (locked GO-03 wire rule). getBody returns a fresh,
// identical reader, so the request can be replayed; closer closes a file the
// SDK opened from a path, exactly once.
type upload struct {
	contentType   string
	contentLength int64 // -1 = unknown
	fileSize      int64 // -1 = unknown
	body          io.ReadCloser
	getBody       func() (io.ReadCloser, error)
	closer        *ownCloser
}

// ownCloser closes a file the SDK opened from a path — never a caller's
// reader or file (D-07). close is safe to call more than once.
type ownCloser struct {
	f    *os.File
	done bool
}

func (o *ownCloser) close() {
	if o != nil && o.f != nil && !o.done {
		o.done = true
		_ = o.f.Close()
	}
}

// uploadBody is the request body: the framed multi-reader plus a Close that
// only ever closes a file the SDK opened from a path.
type uploadBody struct {
	io.Reader
	closer *ownCloser
}

// Close closes a file the SDK opened from a path; a caller's reader is never
// closed (D-07).
func (b *uploadBody) Close() error {
	b.closer.close()
	return nil
}

// exactReader reads at most total bytes from src. When src ends before the
// measured length has been delivered it fails with an error wrapping
// errShortRead, so a file that shrank during upload can never silently
// undersend, and one that grew can never over-send (D-07).
type exactReader struct {
	src   io.Reader
	left  int64
	total int64
	err   error
}

func (e *exactReader) Read(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	if e.left <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > e.left {
		p = p[:e.left]
	}
	n, err := e.src.Read(p)
	e.left -= int64(n)
	switch {
	case err == io.EOF && e.left > 0:
		e.err = fmt.Errorf("%w: %d of %d measured bytes missing", errShortRead, e.left, e.total)
	case err != nil:
		e.err = err
	}
	if e.err != nil {
		return n, e.err
	}
	return n, nil
}

// measurement carries the outcome of the D-07 length measurement: n is the
// number of bytes the source still yields (-1 when unknown), start is the
// position a GetBody rewind must restore, and canSeek reports whether that
// rewind is possible.
type measurement struct {
	n       int64
	start   int64
	canSeek bool
}

// lener is a source measurable through a Len method (D-07), such as
// *bytes.Reader or *bytes.Buffer.
type lener interface{ Len() int }

// measureLength implements the D-07 order: a regular *os.File is measured by
// Stat minus its current offset, then any io.Seeker by an end seek from its
// current position (restored before returning), then a type with Len. A
// source is only ever moved relative to its own position, never rewound to an
// absolute start; an unmeasurable source reports n -1.
func measureLength(r io.Reader) measurement {
	if f, ok := r.(*os.File); ok {
		if st, err := f.Stat(); err == nil && st.Mode().IsRegular() {
			cur, cerr := f.Seek(0, io.SeekCurrent)
			if cerr != nil || st.Size() < cur {
				return measurement{n: -1}
			}
			return measurement{n: st.Size() - cur, start: cur, canSeek: true}
		}
	}
	if s, ok := r.(io.Seeker); ok {
		cur, err := s.Seek(0, io.SeekCurrent)
		if err == nil {
			end, err := s.Seek(0, io.SeekEnd)
			_, _ = s.Seek(cur, io.SeekStart)
			if err == nil && end >= cur {
				return measurement{n: end - cur, start: cur, canSeek: true}
			}
		}
	}
	if l, ok := r.(lener); ok {
		return measurement{n: int64(l.Len())}
	}
	return measurement{n: -1}
}

// prepareUpload validates the call and frames the upload for every input kind
// (D-06, D-07). The part name follows the strict filename rule: WithFilename
// wins verbatim (also over a path's basename), else a path's basename, else
// the name carried by the File; an empty final name is ErrMissingFilename —
// there is no default filename. Every validation happens before openFile is
// called or any byte is read.
func prepareUpload(file File, instr Instruction, qs querySettings) (*upload, error) {
	body, err := bodyJSON(instr, qs.mode)
	if err != nil {
		return nil, err
	}

	var partName string
	switch file.kind {
	case fileBytes:
		partName = file.name
	case filePath:
		if file.path == "" {
			return nil, ErrNoFile
		}
		partName = filepath.Base(file.path)
	case fileReader:
		if file.reader == nil {
			return nil, ErrNoFile
		}
		partName = file.name
	default:
		return nil, ErrNoFile
	}
	if qs.filename != nil {
		partName = *qs.filename
	}
	if partName == "" {
		return nil, ErrMissingFilename
	}

	// Validation is complete: open and measure only now.
	var source io.Reader
	var own *ownCloser
	var m measurement
	switch file.kind {
	case fileBytes:
		source = bytes.NewReader(file.data)
		m = measurement{n: int64(len(file.data))}
	case filePath:
		f, oerr := openFile(file.path)
		if oerr != nil {
			return nil, fmt.Errorf("docql: %w", oerr)
		}
		own = &ownCloser{f: f}
		st, serr := f.Stat()
		if serr != nil {
			own.close()
			return nil, fmt.Errorf("docql: %w", serr)
		}
		if st.IsDir() {
			own.close()
			return nil, fmt.Errorf("docql: %q is a directory: %w", file.path, fs.ErrInvalid)
		}
		source = f
		m = measureLength(f)
	default: // fileReader
		source = file.reader
		m = measureLength(source)
	}

	boundary := newBoundary()
	preamble, epilogue, err := frame(partName, body, boundary)
	if err != nil {
		return nil, err
	}

	contentLength := int64(-1)
	fileSize := int64(-1)
	var fileSrc io.Reader = source
	if m.n >= 0 {
		fileSrc = &exactReader{src: source, left: m.n, total: m.n}
		contentLength = int64(len(preamble)) + m.n + int64(len(epilogue))
		fileSize = m.n
	}

	var getBodyFn func() (io.ReadCloser, error)
	switch {
	case file.kind == fileBytes:
		getBodyFn = func() (io.ReadCloser, error) {
			return io.NopCloser(io.MultiReader(
				bytes.NewReader(preamble),
				bytes.NewReader(file.data),
				bytes.NewReader(epilogue),
			)), nil
		}
	case file.kind == filePath:
		// A path replay reopens the file: the transport closes the original
		// request body (and with it the SDK-opened handle) before it would
		// ever call GetBody, so a rewind of that handle cannot work.
		getBodyFn = func() (io.ReadCloser, error) {
			f, oerr := openFile(file.path)
			if oerr != nil {
				return nil, oerr
			}
			if _, serr := f.Seek(m.start, io.SeekStart); serr != nil {
				_ = f.Close()
				return nil, serr
			}
			return &uploadBody{
				Reader: io.MultiReader(
					bytes.NewReader(preamble),
					&exactReader{src: f, left: m.n, total: m.n},
					bytes.NewReader(epilogue),
				),
				closer: &ownCloser{f: f},
			}, nil
		}
	case m.canSeek:
		// A caller's seeker is rewound to the recorded start offset and read
		// again from there.
		seeker := source.(io.Seeker)
		getBodyFn = func() (io.ReadCloser, error) {
			if _, err := seeker.Seek(m.start, io.SeekStart); err != nil {
				return nil, err
			}
			return io.NopCloser(io.MultiReader(
				bytes.NewReader(preamble),
				&exactReader{src: source, left: m.n, total: m.n},
				bytes.NewReader(epilogue),
			)), nil
		}
	}

	return &upload{
		contentType:   "multipart/form-data; boundary=" + boundary,
		contentLength: contentLength,
		fileSize:      fileSize,
		body: &uploadBody{
			Reader: io.MultiReader(bytes.NewReader(preamble), fileSrc, bytes.NewReader(epilogue)),
			closer: own,
		},
		getBody: getBodyFn,
		closer:  own,
	}, nil
}
