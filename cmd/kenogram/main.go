package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/idolum-ai/kenogram/internal/decl"
	"github.com/idolum-ai/kenogram/internal/plan"
	"github.com/idolum-ai/kenogram/internal/version"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printHelp(stderr)
		return 2
	}
	switch args[0] {
	case "up":
		return runUp(args[1:], stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, version.String())
		return 0
	case "help", "--help", "-h":
		printHelp(stdout)
		return 0
	default:
		fmt.Fprintln(stderr, "unknown command:", args[0])
		printHelp(stderr)
		return 2
	}
}

func runUp(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "stop after rendering the plan")
	jsonOutput := fs.Bool("json", false, "render machine-readable JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: kenogram up --dry-run [--json] <file>")
		return 2
	}
	path := fs.Arg(0)
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(stderr, "read declaration:", err)
		return 1
	}
	d, err := decl.Parse(data)
	if err != nil {
		fmt.Fprintln(stderr, "parse declaration:", err)
		return 1
	}
	result, err := plan.Build(d, path, data)
	if err != nil {
		fmt.Fprintln(stderr, "validate declaration:", err)
		return 1
	}
	if *jsonOutput {
		encoded, err := plan.JSON(result)
		if err != nil {
			fmt.Fprintln(stderr, "render plan:", err)
			return 1
		}
		if _, err := stdout.Write(encoded); err != nil {
			fmt.Fprintln(stderr, "write plan:", err)
			return 1
		}
	} else if err := plan.RenderText(stdout, result); err != nil {
		fmt.Fprintln(stderr, "render plan:", err)
		return 1
	}
	if !*dryRun {
		fmt.Fprintln(stderr, "materialization is not implemented; no world was changed (use --dry-run for the implemented M1 boundary)")
		return 1
	}
	return 0
}

func printHelp(w io.Writer) {
	fmt.Fprint(w, `Usage:
  kenogram up --dry-run [--json] <file>  validate and plan (implemented)
  kenogram version                     show build provenance
  kenogram help                        show this help

Planned: up materialization, down, destroy, enter, status, allow, worlds.
`)
}
