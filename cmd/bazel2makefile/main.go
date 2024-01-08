package main

import (
	"log"
	"os"

	"github.com/goccy/go-yaml"
	"github.com/jessevdk/go-flags"

	"github.com/goccy/go-wasmbind-tools/bazelmake"
)

type Option struct {
	Config string `description:"specify config.yaml" short:"c" long:"config" default:"config.yaml"`
}

func run(args []string, opt *Option) error {
	cfgFile, err := os.ReadFile(opt.Config)
	if err != nil {
		return err
	}
	var cfg bazelmake.Config
	if err := yaml.UnmarshalWithOptions(cfgFile, &cfg, yaml.Strict()); err != nil {
		return err
	}
	resolver := bazelmake.NewResolver(cfg)
	libs, err := resolver.Resolve()
	if err != nil {
		return err
	}
	makefile, err := bazelmake.CreateMakefile(cfg, libs)
	if err != nil {
		return err
	}
	if err := os.WriteFile("Makefile", makefile, 0o600); err != nil {
		return err
	}
	return nil
}

func main() {
	var opt Option
	parser := flags.NewParser(&opt, flags.Default)
	args, err := parser.Parse()
	if err != nil {
		return
	}
	if err := run(args, &opt); err != nil {
		log.Print(err)
	}
}
