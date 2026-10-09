package docql

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
)

// File is one sealed document input for QueryDocument (D-06), built with
// FileBytes (and, in later releases, FilePath and FileReader). The zero value
// is not a valid file.
type File struct {
	kind fileKind
	data []byte
	name string
}

// FileBytes builds a File from raw contents (D-06). name names the uploaded
// file part; a call can override it with WithFilename. Nil or empty data is a
// zero-length file; there is no client-side size check, the server is the
// authority.
func FileBytes(data []byte, name string) File {
	return File{kind: fileBytes, data: data, name: name}
}
