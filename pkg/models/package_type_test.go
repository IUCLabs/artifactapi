package models_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IUCLabs/artifactapi/pkg/models"
)

// TestPackageTypeValid parses every package_type_<type>.go and asserts the
// const it declares is registered, so a type file missing its init() fails.
func TestPackageTypeValid(t *testing.T) {
	files, err := filepath.Glob("package_type_*.go")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				for _, v := range spec.(*ast.ValueSpec).Values {
					lit := v.(*ast.BasicLit)
					pt := models.PackageType(strings.Trim(lit.Value, `"`))
					if !pt.Valid() {
						t.Errorf("%s: %q is not registered", f, pt)
					}
					if want := "package_type_" + pt.String() + ".go"; f != want {
						t.Errorf("%s declares %q, want it in %s", f, pt, want)
					}
					seen++
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("found no package types")
	}
}

func TestPackageTypeInvalid(t *testing.T) {
	invalid := []string{"", "bogus", "Docker", "HELM"}
	for _, s := range invalid {
		pt := models.PackageType(s)
		if pt.Valid() {
			t.Errorf("expected %q to be invalid", s)
		}
	}
}

func TestParsePackageType(t *testing.T) {
	pt, err := models.ParsePackageType("docker")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pt != models.PackageDocker {
		t.Errorf("expected docker, got %q", pt)
	}

	_, err = models.ParsePackageType("nope")
	if err == nil {
		t.Fatal("expected error for unknown type")
	}
}

func TestPackageTypeString(t *testing.T) {
	if models.PackageGoProxy.String() != "goproxy" {
		t.Errorf("expected 'goproxy', got %q", models.PackageGoProxy.String())
	}
}
