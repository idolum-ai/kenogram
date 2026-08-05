package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/idolum-ai/kenogram/internal/backend"
	"github.com/idolum-ai/kenogram/internal/job"
	"github.com/idolum-ai/kenogram/internal/jobenv"
	"github.com/idolum-ai/kenogram/internal/jobpodman"
)

func init() {
	governedJobRuntime = func() job.Runtime { return jobpodman.New(nil) }
}

func runGovernedJobHelper(args []string, stdin io.Reader, stderr io.Writer) (int, bool) {
	if len(args) == 0 {
		return 0, false
	}
	switch args[0] {
	case "_job-hold":
		if len(args) != 1 {
			return 125, true
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		<-ctx.Done()
		return 0, true
	case "_job-exec":
		if len(args) < 2 {
			return 125, true
		}
		items, err := jobenv.Decode(stdin)
		if err != nil {
			fmt.Fprintln(stderr, "job environment handoff is invalid")
			return 125, true
		}
		os.Clearenv()
		for _, item := range items {
			if err := os.Setenv(item.Name, string(item.Value)); err != nil {
				fmt.Fprintln(stderr, "job environment could not be established")
				return 125, true
			}
		}
		if err := syscall.Exec(args[1], args[1:], os.Environ()); err != nil {
			fmt.Fprintln(stderr, "job target could not be executed")
			return 126, true
		}
		return 0, true
	case "_job-collect":
		if len(args) != 8 {
			return 125, true
		}
		maximumEntries, entriesErr := strconv.ParseInt(args[6], 10, 64)
		maximumBytes, bytesErr := strconv.ParseInt(args[7], 10, 64)
		if entriesErr != nil || bytesErr != nil || maximumEntries < 1 || maximumBytes < 1 {
			fmt.Fprintln(stderr, "job artifact bounds are invalid")
			return 125, true
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := jobpodman.CollectArtifacts(ctx, backend.New(nil), args[1], args[2], args[3], args[4], args[5], maximumEntries, maximumBytes); err != nil {
			fmt.Fprintln(stderr, "job artifact collection failed")
			return 125, true
		}
		return 0, true
	default:
		return 0, false
	}
}
