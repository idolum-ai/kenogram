// Command releaseprovenance binds one packaged executable and its retained
// machine-readable provenance to verifier-owned release coordinates.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/idolum-ai/kenogram/internal/releaseprovenance"
)

func main() {
	set := flag.NewFlagSet("releaseprovenance", flag.ExitOnError)
	executable := set.String("executable", "", "packaged executable")
	provenance := set.String("provenance", "", "retained version --json output")
	version := set.String("version", "", "expected release version")
	commit := set.String("commit", "", "expected full source commit")
	sourceDate := set.String("source-date", "", "expected canonical UTC source date")
	goos := set.String("goos", "", "expected target operating system")
	goarch := set.String("goarch", "", "expected target architecture")
	set.Parse(os.Args[1:])
	if set.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "releaseprovenance: positional arguments are not accepted")
		os.Exit(2)
	}
	_, err := releaseprovenance.Verify(*executable, *provenance, releaseprovenance.Expected{
		Version: *version, Commit: *commit, SourceDate: *sourceDate, GOOS: *goos, GOARCH: *goarch,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "releaseprovenance:", err)
		os.Exit(1)
	}
	fmt.Println("PASS")
}
