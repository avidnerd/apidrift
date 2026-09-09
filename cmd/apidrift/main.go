// Command apidrift proxies a service's outbound HTTP calls, learns the shape of
// the JSON responses it sees, and reports when that shape changes.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"
)

// Exit codes. Distinguishing usage from failure lets a script tell "you called
// it wrong" from "it ran and something went wrong".
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// errUsage signals that the command line was wrong, rather than the work.
var errUsage = errors.New("usage")

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run holds the real entrypoint so tests can drive it with their own streams.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return exitUsage
	}

	var err error
	switch args[0] {
	case "serve":
		err = serveCmd(args[1:], stdout, stderr)
	case "report":
		err = reportCmd(args[1:], stdout, stderr)
	case "eval":
		err = evalCmd(args[1:], stdout, stderr)
	case "assess":
		err = assessCmd(args[1:], stdout, stderr)
	case "watch":
		err = watchCmd(args[1:], stdout, stderr)
	case "fix":
		err = fixCmd(args[1:], stdout, stderr)
	case "demo":
		err = demoCmd(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	default:
		fmt.Fprintf(stderr, "apidrift: unknown command %q\n\n", args[0])
		usage(stderr)
		return exitUsage
	}

	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, flag.ErrHelp):
		return exitOK
	case errors.Is(err, errUsage):
		fmt.Fprintf(stderr, "apidrift: %v\n", err)
		return exitUsage
	default:
		fmt.Fprintf(stderr, "apidrift: %v\n", err)
		return exitError
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `apidrift detects structural changes in the JSON responses of an upstream API.

usage:
  apidrift watch  -spec <url> -repo <dir>    diff an API spec against last run and open a PR
  apidrift serve  --upstream <url> [flags]   run the proxy and analyse what passes through
  apidrift report [flags]                    print the findings from a running proxy
  apidrift eval   -v1 <spec> -v2 <spec>      measure the detector against two spec versions
  apidrift assess [flags]                    ask a model what the findings mean and what they break
  apidrift fix    [flags]                    patch your code for the findings and open a pull request
  apidrift demo                              run the whole pipeline against a fake API that breaks

run "apidrift <command> -h" for the flags of each.
`)
}

// parseError classifies a flag-parsing failure. A malformed command line is a
// usage error, not a run failure -- but flag.ErrHelp means the user asked for
// help and got it, which is success, so it is passed through untouched.
func parseError(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return err
	}
	return fmt.Errorf("%w: %v", errUsage, err)
}

// parseDuration is time.ParseDuration, wrapped so tests can use it without
// importing time for one call.
func parseDuration(s string) (time.Duration, error) { return time.ParseDuration(s) }
