package bazelmake_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/goccy/go-wasmbind-tools/bazelmake"
)

func TestResolver(t *testing.T) {
	cfg, err := bazelmake.LoadConfig(filepath.Join("testdata", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	makefile, err := bazelmake.CreateMakefile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("Makefile", makefile, 0o600); err != nil {
		t.Fatal(err)
	}
}
