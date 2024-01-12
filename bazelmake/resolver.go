package bazelmake

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"reflect"
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
		ret = append(ret, filepath.Join(root, lib.File.Library.Root, lib.File.Path, src))
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
	pathToVariables  map[string]map[string]any
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
		pathToVariables:  make(map[string]map[string]any),
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
			r.pathToVariables[path] = make(map[string]any)
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
			relPath = strings.TrimPrefix(relPath, "/")
			file := &File{
				Path:        relPath,
				Library:     lib,
				CCLibraries: libs,
				cclibMap:    cclibMap,
				otherLibMap: otherLibMap,
			}
			r.libraryFileMap[lib][relPath] = file
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
		switch v := stmt.(type) {
		case *build.AssignExpr:
			variableName := r.resolveIdentifier(path, v.LHS)
			r.pathToVariables[path][variableName] = r.resolveExpr(path, v.RHS)
		}
		callExpr, ok := stmt.(*build.CallExpr)
		if !ok {
			continue
		}
		name := r.getText(callExpr.X)
		switch name {
		case "cc_library":
			libs = append(libs, r.resolveCCLibrary(path, callExpr.List))
		case "cc_proto_library":
			libs = append(libs, r.resolveCCProtoLibrary(path, callExpr.List))
		case "proto_library":
			libs = append(libs, r.resolveProtoLibrary(path, callExpr.List))
		case "configure_make":
			libs = append(libs, r.resolveConfigureMake(path, callExpr.List))
		case "filegroup":
			libs = append(libs, r.resolveFilegroup(path, callExpr.List))
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

func (r *Resolver) resolveCCLibrary(path string, list []build.Expr) *CCLibrary {
	var ret CCLibrary
	for _, item := range list {
		assignExpr, ok := item.(*build.AssignExpr)
		if !ok {
			continue
		}
		kind := r.getText(assignExpr.LHS)
		value := r.resolveExpr(path, assignExpr.RHS)
		switch kind {
		case "name":
			ret.Name = r.toString(value)
		case "hdrs":
			ret.Headers = r.filterSource(r.toStrings(value))
		case "srcs":
			ret.Sources = r.filterSource(r.toStrings(value))
		case "deps":
			ret.Dependencies = r.toStrings(value)
		case "copts":
			ret.Options = r.toStrings(value)
		}
	}
	return &ret
}

func (r *Resolver) resolveIdentifier(path string, expr build.Expr) string {
	switch v := expr.(type) {
	case *build.Ident:
		return v.Name
	case *build.DotExpr:
		return strings.Join(append(r.toStrings(r.resolveExpr(path, v.X)), v.Name), ".")
	}
	return ""
}

func (r *Resolver) resolveExpr(path string, expr build.Expr) any {
	switch v := expr.(type) {
	case *build.Ident:
		return r.pathToVariables[path][v.Name]
	case *build.StringExpr:
		return v.Value
	case *build.BinaryExpr:
		left := r.resolveExpr(path, v.X)
		right := r.resolveExpr(path, v.Y)
		switch v.Op {
		case "+":
			if left == nil && right == nil {
				return nil
			}
			if left == nil {
				return right
			}
			if right == nil {
				return left
			}
			if reflect.TypeOf(left).Kind() == reflect.Slice {
				return append(append([]string{}, left.([]string)...), right.([]string)...)
			}
			return left.(string) + right.(string)
		default:
			log.Printf("binary: %s: %v, %v", v.Op, v.X, v.Y)
		}
		return []string{}
	case *build.CallExpr:
		fn := r.resolveIdentifier(path, v.X)
		switch fn {
		case "glob":
			fileMap := make(map[string]struct{})
			dirName := filepath.Dir(path)
			for _, vv := range v.List {
				for _, p := range r.toStrings(r.resolveExpr(path, vv)) {
					globPath := filepath.Join(filepath.Dir(path), p)
					matches, err := filepath.Glob(globPath)
					if err != nil {
						log.Fatalf("failed to run glob(%s)", globPath)
					}
					for _, match := range matches {
						if finfo, _ := os.Stat(match); finfo.IsDir() {
							submatches, err := filepath.Glob(fmt.Sprintf("%s/*", match))
							if err != nil {
								log.Fatalf("failed to run %s*", match)
							}
							for _, submatch := range submatches {
								fileMap[strings.TrimPrefix(submatch, dirName+"/")] = struct{}{}
							}
						}
						fileMap[strings.TrimPrefix(match, dirName+"/")] = struct{}{}
					}
				}
			}
			ret := make([]string, 0, len(fileMap))
			for file := range fileMap {
				ret = append(ret, file)
			}
			sort.Strings(ret)
			return ret
		case "select":
			return []string{}
		}
		log.Printf("%s(%v)", r.getText(v.X), v.List)
		return nil
	case *build.ListExpr:
		var ret []string
		for _, vv := range v.List {
			ret = append(ret, r.toStrings(r.resolveExpr(path, vv))...)
		}
		return ret
	case *build.LiteralExpr:
		return []string{v.Token}
	case *build.AssignExpr:
		variableName := r.resolveIdentifier(path, v.LHS)
		r.pathToVariables[path][variableName] = r.resolveExpr(path, v.RHS)
	case *build.Comprehension:
		var ret []string
		for _, clause := range v.Clauses {
			ret = append(ret, r.resolveClause(path, clause, v.Body)...)
		}
		return ret
	case *build.DictExpr:
		return nil
	default:
		log.Printf("unknown ast: %T", v)
	}
	return []string{}
}

func (r *Resolver) resolveClause(path string, clause build.Expr, body build.Expr) []string {
	switch v := clause.(type) {
	case *build.ForClause:
		var ret []string
		variableName := r.resolveIdentifier(path, v.Vars)
		for _, iter := range r.toStrings(r.resolveExpr(path, v.X)) {
			r.pathToVariables[path][variableName] = iter
			ret = append(ret, r.toStrings(r.resolveExpr(path, body))...)
		}
		return ret
	}
	log.Printf("unsupported clause type: %T", clause)
	return nil
}

func (r *Resolver) resolveCCProtoLibrary(path string, list []build.Expr) *CCLibrary {
	var ret CCLibrary
	for _, item := range list {
		assignExpr, ok := item.(*build.AssignExpr)
		if !ok {
			continue
		}
		kind := r.getText(assignExpr.LHS)
		value := r.resolveExpr(path, assignExpr.RHS)
		switch kind {
		case "name":
			ret.Name = r.toString(value)
		case "deps":
			ret.Dependencies = r.toStrings(value)
		}
	}
	return &ret
}

func (r *Resolver) resolveProtoLibrary(path string, list []build.Expr) *CCLibrary {
	var ret CCLibrary
	for _, item := range list {
		assignExpr, ok := item.(*build.AssignExpr)
		if !ok {
			continue
		}
		kind := r.getText(assignExpr.LHS)
		value := r.resolveExpr(path, assignExpr.RHS)
		switch kind {
		case "name":
			ret.Name = r.toString(value)
		case "srcs":
			for _, src := range r.toStrings(value) {
				trimmed := strings.TrimSuffix(src, filepath.Ext(src))
				ret.Sources = append(ret.Sources, fmt.Sprintf("%s.pb.cc", trimmed))
			}
		case "deps":
			ret.Dependencies = r.toStrings(value)
		}
	}
	if ret.Name == "wkt_proto" {
		fmt.Println("found wkt_proto", ret)
		fmt.Println("path to variables", r.pathToVariables[path])
	}
	return &ret
}

func (r *Resolver) resolveConfigureMake(path string, list []build.Expr) *CCLibrary {
	var ret CCLibrary
	for _, item := range list {
		assignExpr, ok := item.(*build.AssignExpr)
		if !ok {
			continue
		}
		kind := r.getText(assignExpr.LHS)
		value := r.resolveExpr(path, assignExpr.RHS)
		switch kind {
		case "name":
			ret.Name = r.toString(value)
		case "srcs":
			ret.Sources = r.filterSource(r.toStrings(value))
		case "lib_source":
			ret.Dependencies = r.toStrings(value)
		}
	}
	return &ret
}

func (r *Resolver) resolveFilegroup(path string, list []build.Expr) *CCLibrary {
	var ret CCLibrary
	for _, item := range list {
		assignExpr, ok := item.(*build.AssignExpr)
		if !ok {
			continue
		}
		kind := r.getText(assignExpr.LHS)
		value := r.resolveExpr(path, assignExpr.RHS)
		switch kind {
		case "name":
			ret.Name = r.toString(value)
		case "srcs":
			ret.Sources = r.filterSource(r.toStrings(value))
		}
	}
	return &ret
}

func (r *Resolver) filterSource(srcs []string) []string {
	filtered := make([]string, 0, len(srcs))
	for _, src := range srcs {
		ext := filepath.Ext(src)
		if ext == ".c" {
			filtered = append(filtered, src)
			continue
		}
		if strings.Contains(ext, ".cc") {
			filtered = append(filtered, src)
			continue
		}
		if strings.Contains(ext, ".cx") {
			filtered = append(filtered, src)
			continue
		}
		if strings.Contains(ext, ".cpp") {
			filtered = append(filtered, src)
			continue
		}
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
			if loc == nil {
				continue
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
				}
			} else if path != "" {
				// @xyz//path/to
				ret.Path = path
				ret.CCLibName = filepath.Base(path)
			} else {
				// @xyz//
				ret.CCLibName = filepath.Base(lib.Root)
			}
		} else {
			// only @xyz
			lib, exists := r.nameToLibraryMap[dep]
			if !exists {
				return nil, fmt.Errorf("failed to find library by name: %s", dep)
			}
			ret.Library = lib
			ret.CCLibName = filepath.Base(lib.Root)
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
		return nil, fmt.Errorf("failed to find cc_library: %+v. cclibMap: %v", loc, file.cclibMap)
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

func (r *Resolver) toString(v any) string {
	if v == nil {
		return ""
	}
	switch s := v.(type) {
	case []string:
		if len(s) != 0 {
			return s[0]
		}
	case string:
		return s
	}
	return ""
}

func (r *Resolver) toStrings(v any) []string {
	if v == nil {
		return nil
	}
	switch s := v.(type) {
	case []string:
		return s
	case string:
		return []string{s}
	}
	return nil
}
