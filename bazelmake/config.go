package bazelmake

import (
	"os"

	"github.com/goccy/go-yaml"
)

type Config struct {
	Root            string                      `yaml:"root"`
	Targets         []*BuildTargetLibraryConfig `yaml:"targets"`
	Ignores         []*IgnoreConfig             `yaml:"ignores"`
	Libraries       []*LibraryConfig            `yaml:"libraries"`
	Output          string                      `yaml:"output"`
	Compiler        string                      `yaml:"compiler"`
	IncludePaths    []string                    `yaml:"include_paths"`
	Sources         []string                    `yaml:"sources"`
	CompilerOptions []string                    `yaml:"compiler_options"`
	LinkerOptions   []string                    `yaml:"linker_options"`
}

type BuildTargetLibraryConfig struct {
	Library string `yaml:"library"`
	Path    string `yaml:"path"`
	Name    string `yaml:"name"`
}

type IgnoreConfig struct {
	Path string `yaml:"path"`
	Name string `yaml:"name"`
}

type LibraryConfig struct {
	Name string `yaml:"name"`
	Root string `yaml:"root"`
}

func LoadConfig(path string) (*Config, error) {
	file, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.UnmarshalWithOptions(file, &cfg, yaml.Strict()); err != nil {
		return nil, err
	}
	return &cfg, nil
}
