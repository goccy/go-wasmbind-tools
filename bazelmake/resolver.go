package bazelmake

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bazelbuild/buildtools/build"
)

type File struct {
	Path        string
	Library     *LibraryConfig
	CCLibraries []*CCLibrary
	cclibMap    map[string]*CCLibrary
	otherLibMap map[string]struct{}
}

func (f *File) FQDN() string {
	return fmt.Sprintf("@%s//%s", f.Library.Name, f.Path)
}

type CCLibrary struct {
	File                 *File
	Name                 string
	Sources              []string
	Headers              []string
	Options              []string
	Dependencies         []string
	ResolvedDependencies []*CCLibrary
}

func (lib *CCLibrary) ObjectFileName() string {
	return strings.ReplaceAll(
		fmt.Sprintf("%s_%s_%s", lib.File.Library.Name, lib.File.Path, lib.Name),
		"/",
		"_",
	)
}

func (lib *CCLibrary) FQDN() string {
	return fmt.Sprintf("%s:%s", lib.File.FQDN(), lib.Name)
}

func (lib *CCLibrary) SourcePaths(root string) []string {
	ret := make([]string, 0, len(lib.Sources))
	for _, src := range lib.Sources {
		ret = append(ret, filepath.Join(root, lib.File.Path, src))
	}
	return ret
}

type LibraryFileMap map[string]*File

type LibraryLocation struct {
	Library   *LibraryConfig
	Path      string
	CCLibName string
	Original  string
}

type Resolver struct {
	cfg              *Config
	nameToLibraryMap map[string]*LibraryConfig
	ignoreMap        map[string]struct{}
	libraryFileMap   map[*LibraryConfig]LibraryFileMap
}

func NewResolver(cfg *Config) *Resolver {
	nameToLibraryMap := make(map[string]*LibraryConfig)
	for _, lib := range cfg.Libraries {
		nameToLibraryMap[lib.Name] = lib
	}
	ignoreMap := make(map[string]struct{})
	for _, ignore := range cfg.Ignores {
		ignoreMap[fmt.Sprintf("%s:%s", ignore.Path, ignore.Name)] = struct{}{}
	}
	return &Resolver{
		cfg:              cfg,
		ignoreMap:        ignoreMap,
		nameToLibraryMap: nameToLibraryMap,
		libraryFileMap:   make(map[*LibraryConfig]LibraryFileMap),
	}
}

func (r *Resolver) Resolve() ([]*CCLibrary, error) {
	for _, lib := range r.cfg.Libraries {
		r.libraryFileMap[lib] = make(LibraryFileMap)
		if lib.Root == "" {
			continue
		}
		if err := r.resolveLibraryBuildFiles(lib); err != nil {
			return nil, err
		}
	}
	for _, fileMap := range r.libraryFileMap {
		names := make([]string, 0, len(fileMap))
		for name := range fileMap {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			file := fileMap[name]
			if err := r.resolveLibraryDependencies(file); err != nil {
				return nil, err
			}
		}
	}
	var targetLibs []*CCLibrary
	for _, target := range r.cfg.Targets {
		lib, exists := r.nameToLibraryMap[target.Library]
		if !exists {
			return nil, fmt.Errorf("failed to find library by name: %s", target.Library)
		}
		cclib, err := r.lookupCCLibraryByLocation(&LibraryLocation{
			Library:   lib,
			Path:      target.Path,
			CCLibName: target.Name,
		})
		if err != nil {
			return nil, err
		}
		if cclib == nil {
			continue
		}
		targetLibs = append(targetLibs, cclib)
	}
	return targetLibs, nil
}

func (r *Resolver) resolveLibraryBuildFiles(lib *LibraryConfig) error {
	libRoot := filepath.Join(r.cfg.Root, lib.Root)
	if err := filepath.Walk(libRoot, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return fmt.Errorf("unexpected error in walk: %w", err)
		}
		switch filepath.Base(path) {
		case "BUILD", "BUILD.bazel":
			libs, otherLibs, err := r.resolveCCLibraries(path)
			if err != nil {
				return err
			}
			cclibMap := make(map[string]*CCLibrary)
			for _, lib := range libs {
				cclibMap[lib.Name] = lib
			}
			otherLibMap := make(map[string]struct{})
			for _, lib := range otherLibs {
				otherLibMap[lib] = struct{}{}
			}
			libDir := filepath.Dir(path)
			relPath := strings.TrimPrefix(libDir, libRoot)
			relPathWithRoot := filepath.Join(lib.Root, strings.TrimPrefix(relPath, "/"))
			file := &File{
				Path:        relPathWithRoot,
				Library:     lib,
				CCLibraries: libs,
				cclibMap:    cclibMap,
				otherLibMap: otherLibMap,
			}
			r.libraryFileMap[lib][relPathWithRoot] = file
			for _, lib := range libs {
				lib.File = file
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func (r *Resolver) resolveCCLibraries(path string) ([]*CCLibrary, []string, error) {
	file, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read file: %w", err)
	}
	tree, err := build.ParseBuild(path, file)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse BUILD file: %w", err)
	}

	var (
		libs      []*CCLibrary
		otherLibs []string
	)
	for _, stmt := range tree.Stmt {
		callExpr, ok := stmt.(*build.CallExpr)
		if !ok {
			continue
		}
		name := r.getText(callExpr.X)
		switch name {
		case "cc_library":
			libs = append(libs, r.resolveCCLibrary(callExpr.List))
		case "cc_proto_library":
			libs = append(libs, r.resolveCCProtoLibrary(callExpr.List))
		case "proto_library":
			libs = append(libs, r.resolveProtoLibrary(callExpr.List))
		default:
			for _, item := range callExpr.List {
				assignExpr, ok := item.(*build.AssignExpr)
				if !ok {
					continue
				}
				kind := r.getText(assignExpr.LHS)
				if kind != "name" {
					continue
				}
				rhs := assignExpr.RHS
				otherLibs = append(otherLibs, r.getText(rhs))
			}
		}
	}
	return libs, otherLibs, nil
}

func (r *Resolver) resolveCCLibrary(list []build.Expr) *CCLibrary {
	var ret CCLibrary
	for _, item := range list {
		assignExpr, ok := item.(*build.AssignExpr)
		if !ok {
			continue
		}
		kind := r.getText(assignExpr.LHS)
		rhs := assignExpr.RHS
		switch kind {
		case "name":
			ret.Name = r.getText(rhs)
		case "hdrs":
			switch v := rhs.(type) {
			case *build.CallExpr:
				log.Printf("%s(%v)", r.getText(v.X), v.List)
			case *build.ListExpr:
				ret.Headers = r.filterSource(r.getTexts(v))
			}
		case "srcs":
			switch v := rhs.(type) {
			case *build.CallExpr:
				log.Printf("%s(%v)", r.getText(v.X), v.List)
			case *build.ListExpr:
				ret.Sources = r.filterSource(r.getTexts(v))
			}
		case "deps":
			switch v := rhs.(type) {
			case *build.BinaryExpr:
			case *build.ListExpr:
				ret.Dependencies = r.getTexts(v)
			}
		case "copts":
			switch v := rhs.(type) {
			case *build.Ident:
				ret.Options = []string{r.getText(v)}
			case *build.ListExpr:
				ret.Options = r.getTexts(v)
			}
		}
	}
	return &ret
}

func (r *Resolver) resolveCCProtoLibrary(list []build.Expr) *CCLibrary {
	var ret CCLibrary
	for _, item := range list {
		assignExpr, ok := item.(*build.AssignExpr)
		if !ok {
			continue
		}
		kind := r.getText(assignExpr.LHS)
		rhs := assignExpr.RHS
		switch kind {
		case "name":
			ret.Name = r.getText(rhs)
		case "deps":
			ret.Dependencies = r.getTexts(rhs.(*build.ListExpr))
		}
	}
	return &ret
}

func (r *Resolver) resolveProtoLibrary(list []build.Expr) *CCLibrary {
	var ret CCLibrary
	for _, item := range list {
		assignExpr, ok := item.(*build.AssignExpr)
		if !ok {
			continue
		}
		kind := r.getText(assignExpr.LHS)
		rhs := assignExpr.RHS
		switch kind {
		case "name":
			ret.Name = r.getText(rhs)
		case "srcs":
			switch v := rhs.(type) {
			case *build.BinaryExpr:
				log.Printf("srcs: %+v", v)
			case *build.ListExpr:
				for _, src := range r.getTexts(v) {
					trimmed := strings.TrimSuffix(src, filepath.Ext(src))
					ret.Sources = append(ret.Sources, fmt.Sprintf("%s.pb.cc", trimmed))
				}
			}
		case "deps":
			switch v := rhs.(type) {
			case *build.Comprehension:
				log.Printf("deps: %+v", v)
			case *build.ListExpr:
				ret.Dependencies = r.getTexts(v)
			}
		}
	}
	return &ret
}

func (r *Resolver) filterSource(srcs []string) []string {
	filtered := make([]string, 0, len(srcs))
	for _, src := range srcs {
		if filepath.Ext(src) == ".inc" {
			continue
		}
		if filepath.Ext(src) == ".h" {
			continue
		}
		filtered = append(filtered, src)
	}
	return filtered
}

func (r *Resolver) resolveLibraryDependencies(file *File) error {
	for _, lib := range file.CCLibraries {
		var depLibs []*CCLibrary
		for _, dep := range lib.Dependencies {
			loc, err := r.resolveLibraryLocation(file, dep)
			if err != nil {
				return err
			}
			depLib, err := r.lookupCCLibraryByLocation(loc)
			if err != nil {
				return err
			}
			if depLib == nil {
				continue
			}
			depLibs = append(depLibs, depLib)
		}
		lib.ResolvedDependencies = depLibs
	}
	return nil
}

func (r *Resolver) resolveLibraryLocation(file *File, dep string) (*LibraryLocation, error) {
	var ret LibraryLocation
	ret.Original = dep
	switch dep[0] {
	case '@':
		dep := strings.TrimPrefix(dep, "@")
		if strings.Contains(dep, "//") {
			parts := strings.Split(dep, "//")
			if len(parts) != 2 {
				return nil, fmt.Errorf("failed to parse library location: %s", dep)
			}
			lib, exists := r.nameToLibraryMap[parts[0]]
			if !exists {
				return nil, fmt.Errorf("failed to find library by name: %s", parts[0])
			}
			path := parts[1]
			ret.Library = lib
			if strings.Contains(path, ":") {
				parts := strings.Split(path, ":")
				if len(parts) != 2 {
					return nil, fmt.Errorf("failed to parse package path of %s", dep)
				}
				ret.CCLibName = parts[1]
				if parts[0] != "" {
					// @xyz//path/to:abcd
					ret.Path = parts[0]
				} else {
					// @xyz//:abcd
					ret.Path = filepath.Base(lib.Root)
				}
			} else if path != "" {
				// @xyz//path/to
				ret.Path = path
				ret.CCLibName = filepath.Base(path)
			} else {
				// @xyz//
				ret.Path = filepath.Base(lib.Root)
				ret.CCLibName = filepath.Base(lib.Root)
			}
		} else {
			// only @xyz
			lib, exists := r.nameToLibraryMap[dep]
			if !exists {
				return nil, fmt.Errorf("failed to find library by name: %s", dep)
			}
			path := filepath.Base(lib.Root)
			ret.Library = lib
			ret.Path = path
			ret.CCLibName = path
		}
	case ':':
		dep := strings.TrimPrefix(dep, ":")
		ret.Library = file.Library
		ret.Path = file.Path
		ret.CCLibName = dep
	case '/':
		if dep[1] != '/' {
			return nil, fmt.Errorf("unexpected dependency value: %s", dep)
		}
		dep := strings.TrimPrefix(dep, "//")
		ret.Library = file.Library
		if strings.Contains(dep, ":") {
			parts := strings.Split(dep, ":")
			if len(parts) != 2 {
				return nil, fmt.Errorf("failed to resolve dependency value: %s", dep)
			}
			ret.Path = parts[0]
			ret.CCLibName = parts[1]
		} else {
			ret.Path = dep
			ret.CCLibName = filepath.Base(dep)
		}
	default:
		ret.Library = file.Library
		ret.Path = file.Path
		ret.CCLibName = dep
	}
	if ret.Path == "" {
		return nil, fmt.Errorf("failed to resolve location path by %s", dep)
	}
	return &ret, nil
}

func (r *Resolver) lookupCCLibraryByLocation(loc *LibraryLocation) (*CCLibrary, error) {
	fileMap, exists := r.libraryFileMap[loc.Library]
	if !exists {
		return nil, fmt.Errorf("failed to find file path map from library reference: %v", loc.Library)
	}
	if len(fileMap) == 0 {
		return nil, nil
	}
	if _, exists := r.ignoreMap[fmt.Sprintf("%s:%s", loc.Path, loc.CCLibName)]; exists {
		return nil, nil
	}
	file, exists := fileMap[loc.Path]
	if !exists {
		if !exists {
			return nil, fmt.Errorf("failed to find file from file map: %+v. file map is %v", loc, fileMap)
		}
	}
	if _, exists := file.otherLibMap[loc.CCLibName]; exists {
		return nil, nil
	}
	cclib, exists := file.cclibMap[loc.CCLibName]
	if !exists {
		return nil, fmt.Errorf("failed to find cc_library: %+v", loc)
	}
	return cclib, nil
}

func (r *Resolver) getText(expr build.Expr) string {
	switch e := expr.(type) {
	case *build.Ident:
		return e.Name
	case *build.StringExpr:
		return e.Value
	}
	return ""
}

func (r *Resolver) getTexts(list *build.ListExpr) []string {
	texts := make([]string, 0, len(list.List))
	for _, value := range list.List {
		texts = append(texts, r.getText(value))
	}
	return texts
}
