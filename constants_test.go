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

// TestConstantsReasons pins the D-08 reason set: exactly 14 distinct values,
// equal to the non-null expect.reason values of the vendored case list.
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
	}
	if len(reasons) != 14 {
		t.Fatalf("the constant slice has %d entries, want 14", len(reasons))
	}
	got := make(map[string]struct{}, len(reasons))
	for _, r := range reasons {
		got[r] = struct{}{}
	}
	if len(got) != 14 {
		t.Fatalf("the constants carry %d distinct values, want 14", len(got))
	}

	kit := loadSDKCases(t)
	want := make(map[string]struct{})
	for _, c := range kit.Cases {
		if c.Expect.Reason != nil {
			want[*c.Expect.Reason] = struct{}{}
		}
	}
	for _, c := range loadDecodeCases(t) {
		if c.Expect.Reason != nil {
			want[*c.Expect.Reason] = struct{}{}
		}
	}
	if len(want) != 14 {
		t.Fatalf("the vendored kit names %d distinct non-null reasons, want 14", len(want))
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

	gotConstants, gotVariables, gotTypes := map[string]bool{}, map[string]bool{}, map[string]bool{}
	gotFunctions, gotMethods, gotFields := map[string]bool{}, map[string]bool{}, map[string]map[string]bool{}
	for typ := range wantFields {
		gotFields[typ] = map[string]bool{}
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
						gotTypes[s.Name.Name] = true
						if st, ok := s.Type.(*ast.StructType); ok {
							if _, tracked := wantFields[s.Name.Name]; tracked {
								for _, field := range st.Fields.List {
									for _, name := range field.Names {
										if ast.IsExported(name.Name) {
											gotFields[s.Name.Name][name.Name] = true
										}
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
								gotConstants[name.Name] = true
							case token.VAR:
								gotVariables[name.Name] = true
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
					gotFunctions[name] = true
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
				gotMethods[recvType+"."+name] = true
			}
		}
	}

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
	compare("constants", wantConstants, gotConstants)
	compare("variables", wantVariables, gotVariables)
	compare("types", wantTypes, gotTypes)
	compare("functions", wantFunctions, gotFunctions)
	compare("methods", wantMethods, gotMethods)
	for typ, wantList := range wantFields {
		compare("fields of "+typ, wantList, gotFields[typ])
	}
}
