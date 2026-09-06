package services

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var catalogAuditModels = map[string]string{"MediaLibraryEntry": "E", "MediaLibraryRecognition": "R", "MediaLibrarySourceAsset": "A"}
var catalogAuditTables = map[string]string{"media_library_entries": "E", "media_library_recognitions": "R", "media_library_source_assets": "A"}
var catalogAuditMethods = map[string]bool{"Model": true, "Table": true, "Raw": true, "Exec": true, "Find": true, "First": true, "Take": true, "Last": true, "Scan": true, "Create": true, "Save": true, "Delete": true, "Updates": true, "Update": true, "UpdateColumn": true, "UpdateColumns": true, "Pluck": true, "Count": true, "Joins": true, "Where": true}

// Legacy helpers may be called without spelling a table or model at the new
// callsite. Keep their callers under exact AST review as well. Matching method
// names is deliberately conservative across receiver types, not an assertion
// of whole-program type/alias analysis.
var catalogAuditSensitiveCalls = map[string]bool{
	"recognizeLibraryUnits": true, "stabilizeExistingRecognitionUnits": true,
	"mergeScopedPan115Catalog": true, "reconcileTMDBCollectionsTx": true,
	"deleteFastScanRows": true, "updateStructureCatalogPaths": true,
	"removeStructureCatalogItems": true, "allocateCatalogAnchors": true,
	"catalogConversionPage": true, "catalogConversionTable": true,
	"generateArtifacts": true, "publishFastPan115Scan": true,
	"completeFastMediaLibraryRecognition": true,
	"unionQuery":                          true, "effectiveSQL": true, "layerSQL": true,
}

var catalogAuditSchemaFiles = map[string]bool{
	"internal/models/models.go":                       true,
	"internal/database/migrations.go":                 true,
	"internal/database/migration_catalog_snapshot.go": true,
}

// Counts are scoped to a named function/receiver or package declaration. Model
// data-only uses are explicit too: adding a runtime query to a permitted pure
// DTO/planner function must not inherit an entire-file exemption.
func catalogAuditReferences(node ast.Node, modelAliases map[string]bool) map[string]int {
	refs := map[string]int{}
	if function, ok := node.(*ast.FuncDecl); ok && catalogAuditSensitiveCalls[function.Name.Name] {
		refs["helper:"+function.Name.Name]++
	}
	variables := map[string]map[string]bool{}
	modelKinds := func(node ast.Node) map[string]bool {
		kinds := map[string]bool{}
		ast.Inspect(node, func(n ast.Node) bool {
			if value, ok := n.(*ast.SelectorExpr); ok {
				if pkg, ok := value.X.(*ast.Ident); ok && modelAliases[pkg.Name] {
					if kind := catalogAuditModels[value.Sel.Name]; kind != "" {
						kinds[kind] = true
					}
				}
			}
			return true
		})
		return kinds
	}
	remember := func(names []*ast.Ident, value ast.Node) {
		kinds := modelKinds(value)
		if len(kinds) > 0 {
			for _, name := range names {
				variables[name.Name] = kinds
			}
		}
	}
	ast.Inspect(node, func(n ast.Node) bool {
		switch value := n.(type) {
		case *ast.Field:
			remember(value.Names, value.Type)
		case *ast.ValueSpec:
			if value.Type != nil {
				remember(value.Names, value.Type)
			}
			for _, item := range value.Values {
				remember(value.Names, item)
			}
		case *ast.AssignStmt:
			for index, item := range value.Rhs {
				if index < len(value.Lhs) {
					if name, ok := value.Lhs[index].(*ast.Ident); ok {
						remember([]*ast.Ident{name}, item)
					}
				}
			}
		}
		return true
	})
	ast.Inspect(node, func(n ast.Node) bool {
		switch value := n.(type) {
		case *ast.SelectorExpr:
			if pkg, ok := value.X.(*ast.Ident); ok && modelAliases[pkg.Name] {
				if kind := catalogAuditModels[value.Sel.Name]; kind != "" {
					refs["model:"+kind]++
				}
			}
		case *ast.BasicLit:
			if value.Kind == token.STRING {
				raw, err := strconv.Unquote(value.Value)
				if err == nil {
					for table, kind := range catalogAuditTables {
						if strings.Contains(raw, table) {
							refs["sql:"+kind]++
						}
					}
				}
			}
		case *ast.CallExpr:
			callName := ""
			switch fun := value.Fun.(type) {
			case *ast.Ident:
				callName = fun.Name
			case *ast.SelectorExpr:
				callName = fun.Sel.Name
			}
			if catalogAuditSensitiveCalls[callName] {
				refs["call:"+callName]++
			}
			method, ok := value.Fun.(*ast.SelectorExpr)
			if !ok || !catalogAuditMethods[method.Sel.Name] {
				return true
			}
			kinds := map[string]bool{}
			for _, arg := range value.Args {
				for kind := range modelKinds(arg) {
					kinds[kind] = true
				}
				ast.Inspect(arg, func(child ast.Node) bool {
					if name, ok := child.(*ast.Ident); ok {
						for kind := range variables[name.Name] {
							kinds[kind] = true
						}
					}
					return true
				})
			}
			for kind := range kinds {
				refs["query:"+method.Sel.Name+":"+kind]++
			}
		}
		return true
	})
	return refs
}

// Hash the normalized syntax of the access-bearing function as well as its
// counted references. Counts alone would miss replacing reader.Entries() with
// an unpinned db.Model() chain using the same local result variable. Go printer
// removes incidental formatting; any semantic edit needs an explicit review.
func catalogAuditSyntaxDigest(node ast.Node) string {
	var code bytes.Buffer
	if err := printer.Fprint(&code, token.NewFileSet(), node); err != nil {
		panic(err)
	}
	digest := sha256.Sum256(code.Bytes())
	return fmt.Sprintf("%x", digest[:8])
}

func catalogAuditKey(file string, decl ast.Decl) string {
	switch value := decl.(type) {
	case *ast.FuncDecl:
		name := value.Name.Name
		if value.Recv != nil && len(value.Recv.List) > 0 {
			receiver := value.Recv.List[0].Type
			if pointer, ok := receiver.(*ast.StarExpr); ok {
				receiver = pointer.X
			}
			if ident, ok := receiver.(*ast.Ident); ok {
				name = ident.Name + "." + name
			}
		}
		return file + ":" + name
	case *ast.GenDecl:
		var names []string
		for _, spec := range value.Specs {
			switch s := spec.(type) {
			case *ast.TypeSpec:
				names = append(names, s.Name.Name)
			case *ast.ValueSpec:
				for _, n := range s.Names {
					names = append(names, n.Name)
				}
			}
		}
		return file + ":declaration(" + strings.Join(names, ",") + ")"
	}
	return file + ":unknown"
}

func collectCatalogRuntimeAccess(root string) (map[string]string, error) {
	access := map[string]string{}
	for _, sub := range []string{"internal", "pkg", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, sub), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			// Only these reviewed schema files are exempt. A newly added model
			// or migration-named file must not silently become a runtime bypass.
			if catalogAuditSchemaFiles[rel] {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			aliases := map[string]bool{}
			for _, imp := range file.Imports {
				name, _ := strconv.Unquote(imp.Path.Value)
				if strings.HasSuffix(name, "/internal/models") {
					alias := "models"
					if imp.Name != nil {
						alias = imp.Name.Name
					}
					aliases[alias] = true
				}
			}
			for _, decl := range file.Decls {
				refs := catalogAuditReferences(decl, aliases)
				if len(refs) == 0 {
					continue
				}
				var values []string
				for name, count := range refs {
					values = append(values, name+"="+strconv.Itoa(count))
				}
				sort.Strings(values)
				access[catalogAuditKey(rel, decl)] = strings.Join(values, ",") + "|" + catalogAuditSyntaxDigest(decl)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return access, nil
}

func TestCatalogRuntimeAccessAllowlist(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate catalog audit source")
	}
	actual, err := collectCatalogRuntimeAccess(filepath.Join(filepath.Dir(source), "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if len(catalogRuntimeAccessAllowlist) == 0 {
		raw, _ := json.MarshalIndent(actual, "", "  ")
		t.Fatalf("catalog access inventory requires review:\n%s", raw)
	}
	for key, refs := range actual {
		if expected, ok := catalogRuntimeAccessAllowlist[key]; !ok || expected != refs {
			t.Errorf("unreviewed catalog access %s: %q (reviewed %q)", key, refs, expected)
		}
	}
	for key := range catalogRuntimeAccessAllowlist {
		if _, ok := actual[key]; !ok {
			t.Errorf("stale catalog access exemption: %s", key)
		}
	}
}

func TestCatalogRuntimeAccessAuditDistinguishesTypesSQLAndQueries(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "sample.go", `package sample
func Read(db DB) { var rows []m.MediaLibraryEntry; db.Find(&rows); db.Raw("SELECT * FROM media_library_recognitions"); _ = "not-a-table" }
func Pure(rows []m.MediaLibrarySourceAsset) { _ = rows }
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	read := catalogAuditReferences(file.Decls[0], map[string]bool{"m": true})
	if read["model:E"] != 1 || read["query:Find:E"] != 1 || read["sql:R"] != 1 {
		t.Fatalf("query escaped audit: %v", read)
	}
	pure := catalogAuditReferences(file.Decls[1], map[string]bool{"m": true})
	if len(pure) != 1 || pure["model:A"] != 1 {
		t.Fatalf("type-only use misclassified: %v", pure)
	}
}

func TestCatalogRuntimeAccessAuditDetectsReceiverBypassWithSameCounts(t *testing.T) {
	parse := func(source string) ast.Decl {
		t.Helper()
		file, err := parser.ParseFile(token.NewFileSet(), "sample.go", source, 0)
		if err != nil {
			t.Fatal(err)
		}
		return file.Decls[0]
	}
	trusted := parse(`package sample; func Read(reader Reader) { var rows []models.MediaLibraryEntry; reader.Entries().Find(&rows) }`)
	bypass := parse(`package sample; func Read(reader Reader) { var rows []models.MediaLibraryEntry; db.Find(&rows) }`)
	before, _ := json.Marshal(catalogAuditReferences(trusted, map[string]bool{"models": true}))
	after, _ := json.Marshal(catalogAuditReferences(bypass, map[string]bool{"models": true}))
	if string(before) != string(after) {
		t.Fatal("fixture must have identical reference counts")
	}
	if catalogAuditSyntaxDigest(trusted) == catalogAuditSyntaxDigest(bypass) {
		t.Fatal("same-count raw database bypass escaped syntax review")
	}
	formatted := parse("package sample\n\nfunc Read(reader Reader) {\n var rows []models.MediaLibraryEntry\n reader.Entries().Find(&rows)\n}\n")
	if catalogAuditSyntaxDigest(trusted) != catalogAuditSyntaxDigest(formatted) {
		t.Fatal("incidental formatting invalidated reviewed syntax")
	}
}

func TestCatalogRuntimeAccessAuditKeepsSiblingFunctionsSeparate(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "sample.go", `package sample
func (r *Reader) Allowed() { r.db.Raw("SELECT * FROM media_library_entries") }
func Bypass(db DB) { db.Exec("DELETE FROM media_library_entries") }
const table = "media_library_source_assets"
`, 0)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, declaration := range file.Decls {
		key := catalogAuditKey("internal/services/sample.go", declaration)
		if keys[key] {
			t.Fatal("function/declaration exemption collision")
		}
		keys[key] = true
		if len(catalogAuditReferences(declaration, nil)) == 0 {
			t.Fatalf("missing raw SQL/constant access: %s", key)
		}
	}
	if !keys["internal/services/sample.go:Reader.Allowed"] || !keys["internal/services/sample.go:Bypass"] || !keys["internal/services/sample.go:declaration(table)"] {
		t.Fatalf("incorrect exact keys: %v", keys)
	}
}

func TestCatalogRuntimeAccessAuditDetectsLegacyHelperCallWithoutModel(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "sample.go", `package sample; func NewCaller(s Service) { s.recognizeLibraryUnits(nil, nil); removeStructureCatalogItems(nil, nil) }`, 0)
	if err != nil {
		t.Fatal(err)
	}
	refs := catalogAuditReferences(file.Decls[0], nil)
	if refs["call:recognizeLibraryUnits"] != 1 || refs["call:removeStructureCatalogItems"] != 1 {
		t.Fatalf("new legacy helper caller escaped review: %v", refs)
	}
}
