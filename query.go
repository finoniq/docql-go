package docql

import "io"

// Mode selects the extraction mode. It is an open type (D-08): ModeFast and
// ModeStandard are the known values, any other value passes through and the
// server validates it.
type Mode string

const (
	// ModeFast asks the engine for the fast extraction mode.
	ModeFast Mode = "fast"
	// ModeStandard asks the engine for the standard extraction mode.
	ModeStandard Mode = "standard"
)

// instructionKind distinguishes an unset instruction from the Query and
// Prompt forms. The zero value is unset, so a var-declared Instruction{} is
// rejected client-side instead of uploading a body with neither field (D-05).
type instructionKind uint8

const (
	instructionNone instructionKind = iota
	instructionQuery
	instructionPrompt
)

// Instruction is one sealed instruction for QueryDocument (D-05): it carries
// either a query or a prompt and can only be built with Query or Prompt, so
// "query xor prompt" holds at compile time. The zero value is not a valid
// instruction. Query("") is a valid query and is sent unchanged.
type Instruction struct {
	kind instructionKind
	text string
}

// Query builds the query form of an Instruction (D-05). An empty query is
// valid and reaches the server unchanged.
func Query(q string) Instruction { return Instruction{kind: instructionQuery, text: q} }

// Prompt builds the prompt form of an Instruction (D-05).
func Prompt(p string) Instruction { return Instruction{kind: instructionPrompt, text: p} }

// fileKind distinguishes an unset File from the supported file inputs. The
// zero value is unset (D-06).
type fileKind uint8

const (
	fileNone fileKind = iota
	fileBytes
	filePath
	fileReader
)

// File is one sealed document input for QueryDocument (D-06), built with
// FileBytes, FilePath or FileReader. The zero value is not a valid file, and
// there is no default filename: an input without a usable name needs
// WithFilename, or the call fails before any I/O.
type File struct {
	kind   fileKind
	data   []byte
	path   string
	reader io.Reader
	name   string
}

// FileBytes builds a File from raw contents (D-06). name names the uploaded
// file part; a call can override it with WithFilename. Nil or empty data is a
// zero-length file; there is no client-side size check, the server is the
// authority.
func FileBytes(data []byte, name string) File {
	return File{kind: fileBytes, data: data, name: name}
}

// FilePath builds a File from a filesystem path (D-06). The file part is
// named after the path's basename unless WithFilename overrides it. The SDK
// opens, measures and closes the file itself; a missing path, an unreadable
// path or a directory fails the call before any byte is sent.
func FilePath(path string) File {
	return File{kind: filePath, path: path}
}

// FileReader builds a File from a caller-owned reader (D-06, D-07). The SDK
// reads from the reader's current position and never closes it. The length is
// measured when it can be (a regular *os.File, an io.Seeker, or a type with
// Len, in that order); an unmeasurable reader is sent with chunked encoding,
// so an oversized upload may then answer 503 instead of the DocQL-shaped 413.
func FileReader(r io.Reader, name string) File {
	return File{kind: fileReader, reader: r, name: name}
}
