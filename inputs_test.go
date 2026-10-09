package docql

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// inputDocument is the shared file payload of the input tests.
var inputDocument = bytes.Repeat([]byte("%PDF-1.4 unit test document line\n"), 4)

// boundaryOf extracts the multipart boundary of a captured request.
func boundaryOf(rec *recordedRequest) string {
	boundary, _ := strings.CutPrefix(rec.header.Get("Content-Type"), "multipart/form-data; boundary=")
	return boundary
}

// filePart returns the file bytes between the file-part headers and the next
// boundary delimiter.
func filePart(t *testing.T, rec *recordedRequest) []byte {
	t.Helper()
	start := bytes.Index(rec.body, []byte("\r\n\r\n"))
	if start < 0 {
		t.Fatalf("no file part header terminator in %d bytes", len(rec.body))
	}
	start += 4
	end := bytes.Index(rec.body[start:], []byte("\r\n--"+boundaryOf(rec)))
	if end < 0 {
		t.Fatal("no closing boundary after the file part")
	}
	return rec.body[start : start+end]
}

// inputFraming computes the expected preamble and epilogue for a file named
// name with the given instruction, using the request's own boundary.
func inputFraming(t *testing.T, name, boundary string, instr Instruction, mode *Mode) ([]byte, []byte) {
	t.Helper()
	body, err := bodyJSON(instr, mode)
	if err != nil {
		t.Fatalf("bodyJSON: %v", err)
	}
	preamble, epilogue, ferr := frame(name, body, boundary)
	if ferr != nil {
		t.Fatalf("frame: %v", ferr)
	}
	return preamble, epilogue
}

// inputSend sends one QueryDocument call with an explicit file and returns
// the next recorded request.
func inputSend(t *testing.T, client *Client, got <-chan *recordedRequest, file File, instr Instruction, callOpts ...QueryOption) *recordedRequest {
	t.Helper()
	if _, err := client.QueryDocument(context.Background(), file, instr, callOpts...); err != nil {
		t.Fatalf("QueryDocument: %v", err)
	}
	return <-got
}

// newInputClient builds a client for the tests of this file.
func newInputClient(t *testing.T, srvURL string, opts ...Option) *Client {
	t.Helper()
	client, err := NewClient(append([]Option{WithAPIKey("input-test-key"), WithAPIURL(srvURL)}, opts...)...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

// trackOpens replaces the openFile seam with a counting wrapper and returns
// the slice of files the SDK opened through it.
func trackOpens(t *testing.T) *[]*os.File {
	t.Helper()
	opened := &[]*os.File{}
	prev := openFile
	openFile = func(path string) (*os.File, error) {
		f, err := os.Open(path)
		if err == nil {
			*opened = append(*opened, f)
		}
		return f, err
	}
	t.Cleanup(func() { openFile = prev })
	return opened
}

// assertClosed reports whether f rejects Stat the way a closed file does.
func assertClosed(t *testing.T, label string, f *os.File) {
	t.Helper()
	if _, err := f.Stat(); err == nil {
		t.Fatalf("%s: the file the SDK opened is still open after the call", label)
	}
}

func TestInputBoundaries(t *testing.T) {
	srv, got := serveCapture(t)
	client := newInputClient(t, srv.URL)
	re := regexp.MustCompile(`^[0-9a-f]{32}$`)

	first := inputSend(t, client, got, FileBytes([]byte("x"), "f.pdf"), Query("q"))
	second := inputSend(t, client, got, FileBytes([]byte("x"), "f.pdf"), Query("q"))
	b1, b2 := boundaryOf(first), boundaryOf(second)
	if b1 == b2 {
		t.Fatalf("two consecutive calls share boundary %q", b1)
	}
	if !re.MatchString(b1) || !re.MatchString(b2) {
		t.Fatalf("boundaries %q / %q must each be 32 lower-case hex characters", b1, b2)
	}
}

func TestInputPathBasename(t *testing.T) {
	opened := trackOpens(t)

	dir := filepath.Join(t.TempDir(), "sub")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	document := filepath.Join(dir, "report.pdf")
	if err := os.WriteFile(document, inputDocument, 0o600); err != nil {
		t.Fatalf("write document: %v", err)
	}

	srv, got := serveCapture(t)
	client := newInputClient(t, srv.URL)

	rec := inputSend(t, client, got, FilePath(document), Query("q"))
	if got := filePart(t, rec); !bytes.Equal(got, inputDocument) {
		t.Fatalf("file part is %d bytes, want the document contents", len(got))
	}
	if !bytes.Contains(rec.body, []byte(`filename="report.pdf"`)) {
		t.Fatal("a path input must name the file part after the basename")
	}
	if len(*opened) != 1 {
		t.Fatalf("openFile was called %d times, want 1", len(*opened))
	}
	assertClosed(t, "after 200", (*opened)[0])

	// WithFilename overrides the basename verbatim.
	*opened = nil
	rec = inputSend(t, client, got, FilePath(document), Query("q"), WithFilename("x.pdf"))
	if !bytes.Contains(rec.body, []byte(`filename="x.pdf"`)) {
		t.Fatal("WithFilename must override the path basename verbatim")
	}
	assertClosed(t, "after override call", (*opened)[0])

	// A handler that drops the connection still leaves the SDK-opened file
	// closed.
	drop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		panic(http.ErrAbortHandler)
	}))
	defer drop.Close()
	dropClient := newInputClient(t, drop.URL)
	*opened = nil
	_, err := dropClient.QueryDocument(context.Background(), FilePath(document), Query("q"))
	var ce *ConnectionError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want *ConnectionError for a dropped connection", err)
	}
	if len(*opened) != 1 {
		t.Fatalf("openFile was called %d times on the failure path, want 1", len(*opened))
	}
	assertClosed(t, "after dropped connection", (*opened)[0])
}

func TestInputPathErrors(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()
	client := newInputClient(t, srv.URL)

	missing := filepath.Join(t.TempDir(), "not-there.pdf")
	_, err := client.QueryDocument(context.Background(), FilePath(missing), Query("q"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want errors.Is(err, fs.ErrNotExist)", err)
	}
	if _, isErr := errorsAsError(err); isErr {
		t.Fatalf("a path-open failure must not be a *docql.Error, got one: %v", err)
	}

	dir := t.TempDir()
	_, err = client.QueryDocument(context.Background(), FilePath(dir), Query("q"))
	if !errors.Is(err, fs.ErrInvalid) {
		t.Fatalf("err = %v, want errors.Is(err, fs.ErrInvalid) for a directory", err)
	}

	if n := requests.Load(); n != 0 {
		t.Fatalf("server saw %d requests, want 0", n)
	}
}

// errorsAsError reports err through *Error via errors.As, for negative
// assertions in TestInputPathErrors.
func errorsAsError(err error) (*Error, bool) {
	var de *Error
	ok := errors.As(err, &de)
	return de, ok
}

func TestInputMissingName(t *testing.T) {
	opened := trackOpens(t)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()
	client := newInputClient(t, srv.URL)
	document := filepath.Join(t.TempDir(), "doc.pdf")
	if err := os.WriteFile(document, inputDocument, 0o600); err != nil {
		t.Fatalf("write document: %v", err)
	}

	cases := []struct {
		name    string
		want    error
		file    File
		instr   Instruction
		callOpt QueryOption
	}{
		{name: "bytes-empty-name", want: ErrMissingFilename, file: FileBytes(inputDocument, "")},
		{name: "reader-empty-name", want: ErrMissingFilename,
			file: FileReader(bytes.NewReader(inputDocument), "")},
		{name: "withfilename-empty", want: ErrMissingFilename,
			file: FilePath(document), instr: Query("q"), callOpt: WithFilename("")},
		{name: "path-empty", want: ErrNoFile, file: FilePath("")},
		{name: "reader-nil", want: ErrNoFile, file: FileReader(nil, "a.pdf")},
		{name: "zero-file", want: ErrNoFile, file: File{}},
		{name: "zero-instruction", want: ErrNoInstruction, file: FileBytes(inputDocument, "f.pdf")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			instr := tc.instr
			if instr.kind == instructionNone && tc.want != ErrNoInstruction {
				instr = Query("q")
			}
			var callOpts []QueryOption
			if tc.callOpt != nil {
				callOpts = append(callOpts, tc.callOpt)
			}
			res, err := client.QueryDocument(context.Background(), tc.file, instr, callOpts...)
			if res != nil {
				t.Fatal("a rejected call must return no result")
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want errors.Is(err, %v)", err, tc.want)
			}
		})
	}

	// A nil context is rejected before everything else.
	//lint:ignore SA1012 the nil-context rejection is the behaviour under test
	if _, err := client.QueryDocument(nil, FileBytes(inputDocument, "f.pdf"), Query("q")); !errors.Is(err, ErrNilContext) {
		t.Fatalf("err = %v, want errors.Is(err, ErrNilContext)", err)
	}

	if n := requests.Load(); n != 0 {
		t.Fatalf("server saw %d requests, want 0", n)
	}
	if len(*opened) != 0 {
		t.Fatalf("openFile was called %d times, want 0: validation precedes any I/O", len(*opened))
	}
}

func TestInputReaderPosition(t *testing.T) {
	srv, got := serveCapture(t)
	client := newInputClient(t, srv.URL)
	const k = 5

	// An *os.File positioned at offset k sends only bytes[k:] and stays open.
	f, err := os.CreateTemp(t.TempDir(), "doc-*.pdf")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	defer f.Close()
	if _, err := f.Write(inputDocument); err != nil {
		t.Fatalf("write document: %v", err)
	}
	if _, err := f.Seek(k, io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	rec := inputSend(t, client, got, FileReader(f, "doc.pdf"), Query("q"))
	if got := filePart(t, rec); !bytes.Equal(got, inputDocument[k:]) {
		t.Fatalf("file part is %d bytes, want the %d bytes after the current position", len(got), len(inputDocument)-k)
	}
	preamble, epilogue := inputFraming(t, "doc.pdf", boundaryOf(rec), Query("q"), nil)
	wantLen := int64(len(preamble) + len(inputDocument) - k + len(epilogue))
	if rec.contentLength != wantLen {
		t.Fatalf("ContentLength = %d, want %d", rec.contentLength, wantLen)
	}
	if _, err := f.Stat(); err != nil {
		t.Fatalf("the caller's file must stay open after the call: %v", err)
	}

	// A bytes.Reader behaves the same after k bytes were read.
	br := bytes.NewReader(inputDocument)
	if _, err := br.Read(make([]byte, k)); err != nil {
		t.Fatalf("read: %v", err)
	}
	rec = inputSend(t, client, got, FileReader(br, "doc.pdf"), Query("q"))
	if got := filePart(t, rec); !bytes.Equal(got, inputDocument[k:]) {
		t.Fatalf("file part is %d bytes, want the bytes after the current position", len(got))
	}
	preamble, epilogue = inputFraming(t, "doc.pdf", boundaryOf(rec), Query("q"), nil)
	wantLen = int64(len(preamble) + len(inputDocument) - k + len(epilogue))
	if rec.contentLength != wantLen {
		t.Fatalf("ContentLength = %d, want %d", rec.contentLength, wantLen)
	}
}

// seekerOnly is measurable through io.Seeker only: it is not an *os.File and
// carries no Len method.
type seekerOnly struct {
	io.Reader
	io.Seeker
}

func TestInputMeasurement(t *testing.T) {
	srv, got := serveCapture(t)
	client := newInputClient(t, srv.URL)

	assertMeasured := func(label string, rec *recordedRequest, data []byte, name string) {
		t.Helper()
		preamble, epilogue := inputFraming(t, name, boundaryOf(rec), Query("q"), nil)
		want := int64(len(preamble) + len(data) + len(epilogue))
		if rec.contentLength != want {
			t.Fatalf("%s: ContentLength = %d, want %d", label, rec.contentLength, want)
		}
		if len(rec.transferEncoding) != 0 {
			t.Fatalf("%s: TransferEncoding = %v, want none for a known length", label, rec.transferEncoding)
		}
		if got := filePart(t, rec); !bytes.Equal(got, data) {
			t.Fatalf("%s: file part is %d bytes, want %d", label, len(got), len(data))
		}
	}

	// An io.Seeker that is not an *os.File.
	br := bytes.NewReader(inputDocument)
	rec := inputSend(t, client, got,
		FileReader(&seekerOnly{Reader: br, Seeker: br}, "doc.pdf"), Query("q"))
	assertMeasured("seeker-only", rec, inputDocument, "doc.pdf")

	// A Len()-only source (bytes.Buffer has no Seek).
	rec = inputSend(t, client, got,
		FileReader(bytes.NewBuffer(inputDocument), "doc.pdf"), Query("q"))
	assertMeasured("len-only", rec, inputDocument, "doc.pdf")

	// A regular *os.File measures by Stat minus the offset.
	f, err := os.CreateTemp(t.TempDir(), "doc-*.pdf")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	defer f.Close()
	if _, err := f.Write(inputDocument); err != nil {
		t.Fatalf("write document: %v", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	rec = inputSend(t, client, got, FileReader(f, "doc.pdf"), Query("q"))
	assertMeasured("regular-file", rec, inputDocument, "doc.pdf")

	// Unmeasurable sources go chunked and still deliver the full framing.
	assertChunked := func(label string, r io.Reader, data []byte) {
		t.Helper()
		rec := inputSend(t, client, got, FileReader(r, "doc.pdf"), Query("q"))
		if rec.contentLength != -1 {
			t.Fatalf("%s: ContentLength = %d, want -1", label, rec.contentLength)
		}
		if len(rec.transferEncoding) != 1 || rec.transferEncoding[0] != "chunked" {
			t.Fatalf("%s: TransferEncoding = %v, want [chunked]", label, rec.transferEncoding)
		}
		preamble, epilogue := inputFraming(t, "doc.pdf", boundaryOf(rec), Query("q"), nil)
		want := append(append(append([]byte{}, preamble...), data...), epilogue...)
		if !bytes.Equal(rec.body, want) {
			t.Fatalf("%s: body is %d bytes, want preamble + %d data bytes + epilogue", label, len(rec.body), len(data))
		}
	}

	assertChunked("plain-reader",
		io.LimitReader(bytes.NewReader(inputDocument), int64(len(inputDocument))), inputDocument)

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if _, err := pw.Write(inputDocument); err != nil {
		t.Fatalf("write pipe: %v", err)
	}
	if err := pw.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	assertChunked("pipe-file", pr, inputDocument)
	if err := pr.Close(); err != nil {
		t.Fatalf("close pipe reader: %v", err)
	}
}

func TestInputGetBody(t *testing.T) {
	srv, got := serveCapture(t)

	run := func(label string, file File) *recordedRequest {
		t.Helper()
		recorder := &getBodyRecorder{inner: http.DefaultTransport}
		client := newInputClient(t, srv.URL, WithHTTPClient(&http.Client{Transport: recorder}))
		rec := inputSend(t, client, got, file, Query("q"))
		if !recorder.hasBody() {
			t.Fatalf("%s: GetBody is nil, want a replayable body", label)
		}
		if !bytes.Equal(recorder.replayBytes(), rec.body) {
			t.Fatalf("%s: GetBody replay is %d bytes, want the %d sent bytes", label, len(recorder.replayBytes()), len(rec.body))
		}
		return rec
	}

	run("bytes", FileBytes(inputDocument, "doc.pdf"))

	p := filepath.Join(t.TempDir(), "doc.pdf")
	if err := os.WriteFile(p, inputDocument, 0o600); err != nil {
		t.Fatalf("write document: %v", err)
	}
	run("path", FilePath(p))

	run("seeker", FileReader(bytes.NewReader(inputDocument), "doc.pdf"))

	// A Len()-only source has a known length but no GetBody.
	recorder := &getBodyRecorder{inner: http.DefaultTransport}
	client := newInputClient(t, srv.URL, WithHTTPClient(&http.Client{Transport: recorder}))
	inputSend(t, client, got, FileReader(bytes.NewBuffer(inputDocument), "doc.pdf"), Query("q"))
	if recorder.hasBody() {
		t.Fatal("a Len()-only source must have no GetBody")
	}
}

// getBodyRecorder captures what req.GetBody would replay, then forwards the
// original request untouched.
type getBodyRecorder struct {
	inner  http.RoundTripper
	mu     sync.Mutex
	replay []byte
	has    bool
}

func (g *getBodyRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	// GetBody is materialized only after the original send: for path and
	// seeker inputs it rewinds the shared source, so calling it before the
	// send would starve the original body. A transport retry uses it exactly
	// this way — after the first write attempt is over.
	resp, err := g.inner.RoundTrip(req)
	g.mu.Lock()
	if req.GetBody != nil {
		g.has = true
		if rc, gerr := req.GetBody(); gerr == nil {
			g.replay, _ = io.ReadAll(rc)
			rc.Close()
		}
	}
	g.mu.Unlock()
	return resp, err
}

func (g *getBodyRecorder) hasBody() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.has
}

func (g *getBodyRecorder) replayBytes() []byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.replay
}

// shortReadSrc declares Len n but always ends immediately.
type shortReadSrc struct{ n int }

func (s *shortReadSrc) Read([]byte) (int, error) { return 0, io.EOF }
func (s *shortReadSrc) Len() int                 { return s.n }

// overReportSrc declares Len 10 but yields all of data in one read.
type overReportSrc struct {
	data []byte
	done bool
}

func (o *overReportSrc) Read(p []byte) (int, error) {
	if o.done {
		return 0, io.EOF
	}
	o.done = true
	return copy(p, o.data), nil
}

func (o *overReportSrc) Len() int { return 10 }

func TestInputShortRead(t *testing.T) {
	srv, got := serveCapture(t)
	client := newInputClient(t, srv.URL)

	_, err := client.QueryDocument(context.Background(),
		FileReader(&shortReadSrc{n: 10}, "doc.pdf"), Query("q"))
	var ce *ConnectionError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %T (%v), want *ConnectionError", err, err)
	}
	if !strings.Contains(err.Error(), "the file changed size during upload") {
		t.Fatalf("err = %q, want the short-read text", err.Error())
	}
	if ce.Timeout() {
		t.Fatal("Timeout() = true, want false for a short read")
	}

	// A source that would yield more than measured sends exactly the measured
	// bytes: never an over-send, never an undersend.
	over := &overReportSrc{data: bytes.Repeat([]byte{0xAB}, 2048)}
	rec := inputSend(t, client, got, FileReader(over, "doc.pdf"), Query("q"))
	if got := filePart(t, rec); !bytes.Equal(got, over.data[:10]) {
		t.Fatalf("file part is %d bytes, want exactly the 10 measured bytes", len(got))
	}
	preamble, epilogue := inputFraming(t, "doc.pdf", boundaryOf(rec), Query("q"), nil)
	if want := int64(len(preamble) + 10 + len(epilogue)); rec.contentLength != want {
		t.Fatalf("ContentLength = %d, want %d", rec.contentLength, want)
	}
}

func TestInputEmptyFile(t *testing.T) {
	srv, got := serveCapture(t)
	client := newInputClient(t, srv.URL)

	for _, data := range [][]byte{nil, {}} {
		rec := inputSend(t, client, got, FileBytes(data, "a.pdf"), Query("q"))
		if got := filePart(t, rec); len(got) != 0 {
			t.Fatalf("file part is %d bytes, want a zero-length part", len(got))
		}
		preamble, epilogue := inputFraming(t, "a.pdf", boundaryOf(rec), Query("q"), nil)
		if want := int64(len(preamble) + len(epilogue)); rec.contentLength != want {
			t.Fatalf("ContentLength = %d, want %d", rec.contentLength, want)
		}
		if len(rec.transferEncoding) != 0 {
			t.Fatalf("TransferEncoding = %v, want none", rec.transferEncoding)
		}
	}
}

func TestInputVerbatimBytes(t *testing.T) {
	srv, got := serveCapture(t)
	client := newInputClient(t, srv.URL)

	content := []byte("x\r\n--abc\r\n" + "--" + "boundarylike\r\nmore--content\r\n")
	rec := inputSend(t, client, got, FileBytes(content, "doc.pdf"), Query("q"))
	if got := filePart(t, rec); !bytes.Equal(got, content) {
		t.Fatalf("file part is %d bytes, want the %d content bytes verbatim", len(got), len(content))
	}
}

func TestInputEmptyQuery(t *testing.T) {
	srv, got := serveCapture(t)
	client := newInputClient(t, srv.URL)

	rec := inputSend(t, client, got, FileBytes([]byte("x"), "f.pdf"), Query(""))
	if got, want := bodyPart(t, rec), map[string]any{"query": ""}; !equalAny(got, want) {
		t.Fatalf("body part = %v, want %v", got, want)
	}

	rec = inputSend(t, client, got, FileBytes([]byte("x"), "f.pdf"), Prompt("p"))
	if got, want := bodyPart(t, rec), map[string]any{"prompt": "p"}; !equalAny(got, want) {
		t.Fatalf("body part = %v, want %v", got, want)
	}

	rec = inputSend(t, client, got, FileBytes([]byte("x"), "f.pdf"), Query("q"), WithMode(ModeStandard))
	if !bytes.Contains(rec.body, []byte(`{"query":"q","params":{"mode":"standard"}`)) {
		t.Fatalf("body must carry the query first and then params.mode, got %q", rec.body)
	}
}
