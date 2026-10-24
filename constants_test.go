package docql

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestConstantsReasons pins the D-08/D-09 reason set: exactly 22 distinct
// values, equal to the union of the non-null reasons of both vendored kit
// files — sdk-cases.json (cases, decodeCases) and job-cases.json (the reason
// table, decodeCases, every job step's reason and embeddedReason, and every
// wait case's reason).
func TestConstantsReasons(t *testing.T) {
	reasons := []string{
		ReasonInvalidAPIKey,
		ReasonUploadTooLarge,
		ReasonUnsupportedMediaType,
		ReasonMissingFilePart,
		ReasonMissingBodyField,
		ReasonBodyNotJSON,
		ReasonMissingQueryOrPrompt,
		ReasonInvalidParams,
		ReasonUnsupportedFileType,
		ReasonCorruptedFile,
		ReasonDocumentExceedsTimeBudget,
		ReasonServiceOverloaded,
		ReasonUpstreamUnavailable,
		ReasonUpstreamAuthFailed,
		ReasonConcurrencyLimitExceeded,
		ReasonRateLimitExceeded,
		ReasonJobNotFound,
		ReasonJobExpired,
		ReasonQueueFull,
		ReasonIdempotencyKeyConflict,
		ReasonIdempotencyInProgress,
		ReasonJobInterrupted,
	}
	if len(reasons) != 22 {
		t.Fatalf("the constant slice has %d entries, want 22", len(reasons))
	}
	got := make(map[string]struct{}, len(reasons))
	for _, r := range reasons {
		got[r] = struct{}{}
	}
	if len(got) != 22 {
		t.Fatalf("the constants carry %d distinct values, want 22", len(got))
	}

	want := make(map[string]struct{})
	add := func(r *string) {
		if r != nil {
			want[*r] = struct{}{}
		}
	}
	kit := loadSDKCases(t)
	for _, c := range kit.Cases {
		add(c.Expect.Reason)
	}
	for _, c := range loadDecodeCases(t) {
		add(c.Expect.Reason)
	}
	jobs := loadJobCases(t)
	for _, e := range jobs.Reasons.Entries {
		r := e.Reason
		add(&r)
	}
	for _, c := range jobs.DecodeCases {
		add(c.Expect.Reason)
	}
	for _, c := range jobs.Cases {
		for _, step := range c.Steps {
			if step.Expect != nil {
				add(step.Expect.Reason)
				add(step.Expect.EmbeddedReason)
			}
		}
	}
	for _, c := range jobs.WaitCases {
		add(c.Expect.Reason)
	}
	if len(want) != 22 {
		t.Fatalf("the vendored kit names %d distinct non-null reasons, want 22", len(want))
	}
	for r := range got {
		if _, ok := want[r]; !ok {
			t.Errorf("constant %q is not a reason of the vendored kit", r)
		}
	}
	for r := range want {
		if _, ok := got[r]; !ok {
			t.Errorf("vendored reason %q has no exported constant", r)
		}
	}
}

// TestSurfaceExported freezes the one-way public surface before the owner
// review (12-07): parsing the package's non-test .go files gives exactly the
// agreed constants, variables, types, functions, methods and struct fields.
func TestSurfaceExported(t *testing.T) {
	wantConstants := []string{
		"Version", "ModeFast", "ModeStandard",
		"ReasonInvalidAPIKey", "ReasonUploadTooLarge", "ReasonUnsupportedMediaType",
		"ReasonMissingFilePart", "ReasonMissingBodyField", "ReasonBodyNotJSON",
		"ReasonMissingQueryOrPrompt", "ReasonInvalidParams", "ReasonUnsupportedFileType",
		"ReasonCorruptedFile", "ReasonDocumentExceedsTimeBudget", "ReasonServiceOverloaded",
		"ReasonUpstreamUnavailable", "ReasonUpstreamAuthFailed",
		"ReasonConcurrencyLimitExceeded", "ReasonRateLimitExceeded",
		"ReasonJobNotFound", "ReasonJobExpired", "ReasonQueueFull",
		"ReasonIdempotencyKeyConflict", "ReasonIdempotencyInProgress", "ReasonJobInterrupted",
	}
	wantVariables := []string{
		"ErrMissingAPIKey", "ErrInvalidAPIURL", "ErrInvalidTimeout", "ErrNilContext",
		"ErrNoInstruction", "ErrNoFile", "ErrMissingFilename",
	}
	wantTypes := []string{
		"Client", "Option", "QueryOption", "Mode", "Instruction", "File",
		"QueryResult", "Error", "ConnectionError",
	}
	wantFunctions := []string{
		"NewClient", "WithAPIKey", "WithAPIURL", "WithTimeout", "WithHTTPClient",
		"WithMode", "WithFilename", "Query", "Prompt", "FilePath", "FileBytes", "FileReader",
	}
	wantMethods := []string{
		"Client.QueryDocument", "Client.String", "Client.GoString",
		"Error.Error", "ConnectionError.Error", "ConnectionError.Unwrap", "ConnectionError.Timeout",
	}
	wantFields := map[string][]string{
		"QueryResult": {"Data", "RequestID", "PageCount", "PagesOCR", "Metadata"},
		"Error":       {"Status", "Reason", "RequestID", "RetryAfter", "Retryable", "Message", "Body"},
	}

	got := parseSurface(t)

	compare := func(kind string, want []string, got map[string]bool) {
		t.Helper()
		gotNames := make([]string, 0, len(got))
		for name := range got {
			gotNames = append(gotNames, name)
		}
		sort.Strings(gotNames)
		wantSorted := append([]string(nil), want...)
		sort.Strings(wantSorted)
		if strings.Join(gotNames, ",") != strings.Join(wantSorted, ",") {
			t.Errorf("%s surface drifted:\n got: %s\nwant: %s", kind, strings.Join(gotNames, ", "), strings.Join(wantSorted, ", "))
		}
	}
	compare("constants", wantConstants, got.constants)
	compare("variables", wantVariables, got.variables)
	compare("types", wantTypes, got.types)
	compare("functions", wantFunctions, got.functions)
	compare("methods", wantMethods, got.methods)
	for typ, wantList := range wantFields {
		compare("fields of "+typ, wantList, got.fields[typ])
	}
}

// packageSurface is the exported surface of the package's non-test .go
// files: constants, variables, types, functions, methods on exported
// receivers, and the exported fields of every exported struct type.
type packageSurface struct {
	constants, variables, types, functions, methods map[string]bool
	fields                                          map[string]map[string]bool
}

// parseSurface walks the package's non-test .go files with go/parser.
func parseSurface(t *testing.T) packageSurface {
	t.Helper()
	got := packageSurface{
		constants: map[string]bool{}, variables: map[string]bool{}, types: map[string]bool{},
		functions: map[string]bool{}, methods: map[string]bool{}, fields: map[string]map[string]bool{},
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list package files: %v", err)
	}
	// Only the package's non-test files freeze the public surface.
	var pkgFiles []string
	for _, f := range files {
		if !strings.HasSuffix(f, "_test.go") {
			pkgFiles = append(pkgFiles, f)
		}
	}
	if len(pkgFiles) == 0 {
		t.Fatal("no package files found")
	}
	fset := token.NewFileSet()
	for _, f := range pkgFiles {
		file, perr := parser.ParseFile(fset, f, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", f, perr)
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						if !ast.IsExported(s.Name.Name) {
							continue
						}
						got.types[s.Name.Name] = true
						if st, ok := s.Type.(*ast.StructType); ok {
							got.fields[s.Name.Name] = map[string]bool{}
							for _, field := range st.Fields.List {
								for _, name := range field.Names {
									if ast.IsExported(name.Name) {
										got.fields[s.Name.Name][name.Name] = true
									}
								}
							}
						}
					case *ast.ValueSpec:
						for _, name := range s.Names {
							if !ast.IsExported(name.Name) {
								continue
							}
							switch d.Tok {
							case token.CONST:
								got.constants[name.Name] = true
							case token.VAR:
								got.variables[name.Name] = true
							}
						}
					}
				}
			case *ast.FuncDecl:
				name := d.Name.Name
				if !ast.IsExported(name) {
					continue
				}
				if d.Recv == nil || len(d.Recv.List) == 0 {
					got.functions[name] = true
					continue
				}
				recvType := ""
				switch r := d.Recv.List[0].Type.(type) {
				case *ast.StarExpr:
					if id, ok := r.X.(*ast.Ident); ok {
						recvType = id.Name
					}
				case *ast.Ident:
					recvType = r.Name
				}
				// A method is public surface only through an exported
				// receiver type; exported names on unexported types
				// (exactReader.Read, uploadBody.Close) stay internal.
				if recvType == "" || !ast.IsExported(recvType) {
					continue
				}
				got.methods[recvType+"."+name] = true
			}
		}
	}
	return got
}

// TestSurfaceV01Subset proves JSDK-03 (additive only): every name of the
// v0.1.0 public surface — the TestSurfaceExported lists as tagged at v0.1.0,
// frozen here verbatim — is still exported. TestSurfaceExported stays exact
// for the current surface; this list never changes.
func TestSurfaceV01Subset(t *testing.T) {
	v01Constants := []string{
		"Version", "ModeFast", "ModeStandard",
		"ReasonInvalidAPIKey", "ReasonUploadTooLarge", "ReasonUnsupportedMediaType",
		"ReasonMissingFilePart", "ReasonMissingBodyField", "ReasonBodyNotJSON",
		"ReasonMissingQueryOrPrompt", "ReasonInvalidParams", "ReasonUnsupportedFileType",
		"ReasonCorruptedFile", "ReasonDocumentExceedsTimeBudget", "ReasonServiceOverloaded",
		"ReasonUpstreamUnavailable", "ReasonUpstreamAuthFailed",
	}
	v01Variables := []string{
		"ErrMissingAPIKey", "ErrInvalidAPIURL", "ErrInvalidTimeout", "ErrNilContext",
		"ErrNoInstruction", "ErrNoFile", "ErrMissingFilename",
	}
	v01Types := []string{
		"Client", "Option", "QueryOption", "Mode", "Instruction", "File",
		"QueryResult", "Error", "ConnectionError",
	}
	v01Functions := []string{
		"NewClient", "WithAPIKey", "WithAPIURL", "WithTimeout", "WithHTTPClient",
		"WithMode", "WithFilename", "Query", "Prompt", "FilePath", "FileBytes", "FileReader",
	}
	v01Methods := []string{
		"Client.QueryDocument", "Client.String", "Client.GoString",
		"Error.Error", "ConnectionError.Error", "ConnectionError.Unwrap", "ConnectionError.Timeout",
	}
	v01Fields := map[string][]string{
		"QueryResult": {"Data", "RequestID", "PageCount", "PagesOCR", "Metadata"},
		"Error":       {"Status", "Reason", "RequestID", "RetryAfter", "Retryable", "Message", "Body"},
	}

	got := parseSurface(t)
	subset := func(kind string, want []string, have map[string]bool) {
		t.Helper()
		for _, name := range want {
			if !have[name] {
				t.Errorf("v0.1.0 %s %s is no longer exported", kind, name)
			}
		}
	}
	subset("constant", v01Constants, got.constants)
	subset("variable", v01Variables, got.variables)
	subset("type", v01Types, got.types)
	subset("function", v01Functions, got.functions)
	subset("method", v01Methods, got.methods)
	for typ, names := range v01Fields {
		subset("field of "+typ, names, got.fields[typ])
	}
}
