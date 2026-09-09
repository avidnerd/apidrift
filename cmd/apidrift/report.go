package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/avidnerd/apidrift/internal/report"
)

// reportConfig is the parsed command line for "apidrift report".
type reportConfig struct {
	admin   string
	asJSON  bool
	timeout time.Duration
}

func parseReportFlags(args []string, stderr io.Writer) (reportConfig, error) {
	var c reportConfig

	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&c.admin, "admin", "http://127.0.0.1:9090", "admin address of a running apidrift serve")
	fs.BoolVar(&c.asJSON, "json", false, "emit JSON instead of the human-readable form")
	fs.DurationVar(&c.timeout, "timeout", 10*time.Second, "how long to wait for the proxy to answer")

	if err := fs.Parse(args); err != nil {
		return c, parseError(err)
	}
	if c.admin == "" {
		return c, fmt.Errorf("%w: report requires -admin", errUsage)
	}
	return c, nil
}

// reportCmd fetches the latest findings from a running proxy and prints them.
//
// It is a client rather than a second analyser: the schemas live in the serving
// process, and a report command that rebuilt them would be reporting on
// different data than the one making the decisions.
func reportCmd(args []string, stdout, stderr io.Writer) error {
	cfg, err := parseReportFlags(args, stderr)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	rep, err := fetchReport(ctx, http.DefaultClient, cfg.admin)
	if err != nil {
		return err
	}

	if cfg.asJSON {
		return report.WriteJSON(stdout, rep)
	}
	return report.WriteText(stdout, rep)
}

// fetchReport retrieves the report from a running proxy's admin listener.
func fetchReport(ctx context.Context, client *http.Client, admin string) (report.Report, error) {
	var zero report.Report

	endpoint := strings.TrimSuffix(admin, "/") + "/findings"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return zero, fmt.Errorf("%w: building request for %s: %v", errUsage, endpoint, err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return zero, unreachable(endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxReportBytes))
	if err != nil {
		return zero, fmt.Errorf("reading %s: %w", endpoint, err)
	}

	if resp.StatusCode != http.StatusOK {
		return zero, fmt.Errorf("%s returned %s: %s", endpoint, resp.Status, describeError(body))
	}

	var rep report.Report
	if err := json.Unmarshal(body, &rep); err != nil {
		return zero, fmt.Errorf("decoding the report from %s: %w", endpoint, err)
	}
	return rep, nil
}

// unreachable explains that nothing is listening, and says what to start.
//
// The commands matter more than the diagnosis here. "is apidrift serve running"
// tells somebody what is wrong without telling them what to do about it, and
// the answer is not guessable: assess and fix are clients that read findings
// from a running proxy, which is not obvious from their names.
func unreachable(endpoint string, cause error) error {
	return fmt.Errorf("contacting %s: %w\n\n"+
		"Nothing is listening there. assess and fix read findings from a running proxy,\n"+
		"so start one in another terminal first:\n\n"+
		"  apidrift demo -serve 127.0.0.1:9090      # fake API with findings, for trying it out\n"+
		"  apidrift serve -upstream <url>           # a real API\n\n"+
		"Then re-run this. Use -admin if the proxy is on a different address",
		endpoint, cause)
}

// maxReportBytes caps what the report command will read from the proxy, so a
// misconfigured -admin pointing at something enormous cannot exhaust memory
// here.
const maxReportBytes = 64 << 20 // 64 MiB

// describeError pulls the message out of the admin surface's error body, or
// falls back to the raw text.
func describeError(body []byte) string {
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.Error != "" {
		return payload.Error
	}
	return strings.TrimSpace(string(body))
}
