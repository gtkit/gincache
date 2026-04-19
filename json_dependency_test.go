package gincache

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

func TestJSONImportsUseGTKitJSON(t *testing.T) {
	t.Parallel()

	files := []string{
		"cache.go",
		"persist/memory.go",
		"persist/redis.go",
		"persist/twolevel.go",
		"persist/ristrettoadapter/adapter.go",
	}

	fset := token.NewFileSet()

	for _, rel := range files {
		path := filepath.FromSlash(rel)
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}

		var hasGTKitJSON bool
		for _, imp := range file.Imports {
			importPath := strings.Trim(imp.Path.Value, `"`)
			if importPath == "encoding/json" {
				t.Fatalf("%s still imports encoding/json", rel)
			}
			if importPath == "github.com/gtkit/json" {
				hasGTKitJSON = true
			}
		}

		if !hasGTKitJSON {
			t.Fatalf("%s does not import github.com/gtkit/json", rel)
		}
	}
}
