package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/redhat-appstudio/lkmabt/internal/cli"
	"github.com/redhat-appstudio/lkmabt/internal/config"
)

// main runs LKMABT and exits with a nonzero status when execution fails.
func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run parses and executes the supplied command-line arguments.
func run(args []string) error {
	req, err := config.Parse(args)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	console := cli.NewConsole()
	return cli.Execute(ctx, req, cli.NewLiveRunner(console, console.Out, console.Interactive()), console)
}
