package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

const usage = "usage: scenarios seed [--manifest seed.yaml] | run [--only S01,S06] [--list] [--verify] | demo"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "scenarios:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	manifest := flags.String("manifest", "seed.yaml", "seed manifest")
	only := flags.String("only", "", "comma-separated scenario ids")
	list := flags.Bool("list", false, "list scenarios")
	verify := flags.Bool("verify", false, "run the verifier after every scenario")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	var selected []scenario
	switch args[0] {
	case "seed", "demo":
	case "run":
		if *list {
			listScenarios()
			return nil
		}
		var err error
		if selected, err = selectScenarios(*only); err != nil {
			return err
		}
	default:
		return errors.New(usage)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	s, err := newStack()
	if err != nil {
		return err
	}
	switch args[0] {
	case "seed":
		return seed(ctx, s, *manifest)
	case "run":
		return runScenarios(ctx, s, selected, *verify)
	}
	return demo(ctx, s)
}
