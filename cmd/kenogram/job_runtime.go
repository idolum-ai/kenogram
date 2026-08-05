package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/idolum-ai/kenogram/internal/backend"
	"github.com/idolum-ai/kenogram/internal/job"
	"github.com/idolum-ai/kenogram/internal/jobenv"
	"github.com/idolum-ai/kenogram/internal/joblifecycle"
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
		items, lifecycleKey, err := jobenv.DecodeLaunch(stdin)
		if err != nil {
			fmt.Fprintln(stderr, "job environment handoff is invalid")
			return 125, true
		}
		environment := make([]string, 0, len(items))
		for _, item := range items {
			environment = append(environment, item.Name+"="+string(item.Value))
		}
		command := exec.Command(args[1], args[2:]...)
		command.Env, command.Stdin, command.Stdout, command.Stderr = environment, nil, os.Stdout, os.Stderr
		startedAt, monotonic := time.Now().UTC(), time.Now()
		if err := command.Start(); err != nil {
			fmt.Fprintln(stderr, "job target could not be executed")
			return 126, true
		}
		waitErr := command.Wait()
		finishedAt := time.Now().UTC()
		record := joblifecycle.Record{Schema: joblifecycle.Schema, StartedAt: startedAt.Format(time.RFC3339Nano), FinishedAt: finishedAt.Format(time.RFC3339Nano), DurationNS: int64(time.Since(monotonic))}
		if waitErr == nil {
			status := int64(0)
			record.ExitStatus = &status
		} else if exit, ok := waitErr.(*exec.ExitError); ok {
			waitStatus, ok := exit.Sys().(syscall.WaitStatus)
			if !ok {
				return 125, true
			}
			if waitStatus.Signaled() {
				signal := int64(waitStatus.Signal())
				record.Signal = &signal
			} else {
				status := int64(waitStatus.ExitStatus())
				record.ExitStatus = &status
			}
		} else {
			return 125, true
		}
		if err := joblifecycle.WriteSlot("/etc/kenogram/target-lifecycle.json", record, lifecycleKey); err != nil {
			fmt.Fprintln(stderr, "job target lifecycle could not be retained")
			return 125, true
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
