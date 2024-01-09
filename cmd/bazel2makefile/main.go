package main

import (
	"log"
	"os"

	"github.com/jessevdk/go-flags"

	"github.com/goccy/go-wasmbind-tools/bazelmake"
)

type Option struct {
	Config string `description:"specify config.yaml" short:"c" long:"config" default:"config.yaml"`
}

func run(args []string, opt *Option) error {
	cfg, err := bazelmake.LoadConfig(opt.Config)
	if err != nil {
		return err
	}
	makefile, err := bazelmake.CreateMakefile(cfg)
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
