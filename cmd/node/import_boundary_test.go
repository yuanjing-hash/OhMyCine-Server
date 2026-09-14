package main

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNodeBinaryDoesNotImportServerControlPlane(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate node package")
	}
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
	command := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", "./cmd/node")
	command.Dir = moduleRoot
	output, err := command.Output()
	if err != nil {
		t.Fatalf("list node dependencies: %v", err)
	}
	for _, dependency := range strings.Fields(string(output)) {
		for _, forbidden := range []string{
			"github.com/yuanjing-hash/OhMyCine-Server/internal/authz",
			"github.com/yuanjing-hash/OhMyCine-Server/internal/database",
			"github.com/yuanjing-hash/OhMyCine-Server/internal/handlers",
			"github.com/yuanjing-hash/OhMyCine-Server/internal/httpserver",
			"github.com/yuanjing-hash/OhMyCine-Server/internal/mediarecognition",
			"github.com/yuanjing-hash/OhMyCine-Server/internal/middleware",
			"github.com/yuanjing-hash/OhMyCine-Server/internal/models",
			"github.com/yuanjing-hash/OhMyCine-Server/internal/services",
			"github.com/yuanjing-hash/OhMyCine-Server/pkg/metadata",
			"github.com/yuanjing-hash/OhMyCine-Server/webui",
		} {
			if dependency == forbidden || strings.HasPrefix(dependency, forbidden+"/") {
				t.Fatalf("ohmycine-node imports forbidden Server control-plane package %s", dependency)
			}
		}
	}
}
