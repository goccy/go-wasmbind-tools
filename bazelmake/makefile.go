package bazelmake

import (
	"bytes"
	_ "embed"
	"path/filepath"
	"sort"
	"strings"

	"text/template"
)

type Makefile struct {
	Root            string
	Output          string
	Compiler        string
	IncludePaths    []string
	CompilerOptions []string
	LinkerOptions   []string
	TargetLibs      []*CCLibrary

	cachedLibraries map[string]struct{}
}

type NameAndPath struct {
	Name string
	Path string
}

func (m *Makefile) Sources() []*NameAndPath {
	var ret []*NameAndPath
	for _, lib := range m.TargetLibs {
		for _, src := range lib.SourcePaths(m.Root) {
			name := strings.ReplaceAll(strings.TrimSuffix(src, filepath.Ext(src)), "/", "_")
			ret = append(ret, &NameAndPath{
				Name: name,
				Path: src,
			})
		}
		ret = append(ret, m.dependencies(lib)...)
	}
	return ret
}

func (m *Makefile) dependencies(lib *CCLibrary) []*NameAndPath {
	var ret []*NameAndPath
	for _, dep := range lib.ResolvedDependencies {
		fqdn := dep.FQDN()
		if _, exists := m.cachedLibraries[fqdn]; exists {
			continue
		}
		m.cachedLibraries[fqdn] = struct{}{}
		for _, src := range dep.SourcePaths(m.Root) {
			name := strings.ReplaceAll(strings.TrimSuffix(src, filepath.Ext(src)), "/", "_")
			ret = append(ret, &NameAndPath{
				Name: name,
				Path: src,
			})
		}
		ret = append(ret, m.dependencies(dep)...)
	}
	return ret
}

//go:embed templates/Makefile.tmpl
var makefileData []byte

func CreateMakefile(cfg *Config) ([]byte, error) {
	targetLibs, err := NewResolver(cfg).Resolve()
	if err != nil {
		return nil, err
	}

	includePathMap := make(map[string]struct{})
	for _, lib := range cfg.Libraries {
		includePathMap[filepath.Join(cfg.Root, filepath.Dir(lib.Root))] = struct{}{}
		includePathMap[filepath.Join(cfg.Root, lib.Root)] = struct{}{}
	}
	includePaths := make([]string, 0, len(includePathMap))
	for includePath := range includePathMap {
		includePaths = append(includePaths, includePath)
	}
	sort.Strings(includePaths)
	tmpl, err := template.New("").Parse(string(makefileData))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, &Makefile{
		Root:            cfg.Root,
		Output:          cfg.Output,
		Compiler:        cfg.Compiler,
		IncludePaths:    append(includePaths, cfg.IncludePaths...),
		CompilerOptions: cfg.CompilerOptions,
		LinkerOptions:   cfg.LinkerOptions,
		TargetLibs:      targetLibs,
		cachedLibraries: make(map[string]struct{}),
	}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
