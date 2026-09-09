package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/avidnerd/apidrift/internal/detect"
	"github.com/avidnerd/apidrift/internal/eval"
)

// evalConfig is the parsed command line for "apidrift eval".
type evalConfig struct {
	v1, v2           string
	responses        int
	seed             int64
	optionalPresence float64
	asJSON           bool
	latencyCSV       string
	latencyTrials    int
	latencyThreshold float64
	skipLatency      bool
}

func parseEvalFlags(args []string, stderr io.Writer) (evalConfig, error) {
	var c evalConfig

	fs := flag.NewFlagSet("eval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&c.v1, "v1", "", "path to the baseline OpenAPI spec (required)")
	fs.StringVar(&c.v2, "v2", "", "path to the current OpenAPI spec (required)")
	fs.IntVar(&c.responses, "responses", 2000, "responses generated per endpoint per window")
	fs.Int64Var(&c.seed, "seed", 1, "traffic generator seed; runs with the same seed are identical")
	fs.Float64Var(&c.optionalPresence, "optional-presence", 0.7, "probability that an optional field appears")
	fs.BoolVar(&c.asJSON, "json", false, "emit JSON instead of the human-readable summary")
	fs.StringVar(&c.latencyCSV, "latency-csv", "", "write the detection-latency grid to this file, for plotting")
	fs.IntVar(&c.latencyTrials, "latency-trials", 40, "independent draws per cell of the latency sweep")
	fs.Float64Var(&c.latencyThreshold, "latency-threshold", 0.5, "detection probability that counts as \"detected\"")
	fs.BoolVar(&c.skipLatency, "no-latency", false, "skip the detection-latency sweep")

	if err := fs.Parse(args); err != nil {
		return c, parseError(err)
	}
	if c.v1 == "" || c.v2 == "" {
		return c, fmt.Errorf("%w: eval requires -v1 and -v2", errUsage)
	}
	if c.responses < 1 {
		return c, fmt.Errorf("%w: -responses must be at least 1", errUsage)
	}
	return c, nil
}

// evalCmd measures the detector against two versions of a spec.
func evalCmd(args []string, stdout, stderr io.Writer) error {
	cfg, err := parseEvalFlags(args, stderr)
	if err != nil {
		return err
	}

	v1, err := eval.LoadSpec(cfg.v1)
	if err != nil {
		return err
	}
	v2, err := eval.LoadSpec(cfg.v2)
	if err != nil {
		return err
	}

	gen := eval.DefaultGenConfig()
	gen.Seed = cfg.seed
	gen.OptionalPresence = cfg.optionalPresence

	res, runErr := eval.Run(eval.RunConfig{
		V1:        v1,
		V2:        v2,
		Gen:       gen,
		Responses: cfg.responses,
		Detect:    detect.DefaultConfig(),
	})

	// A partial result is still worth printing: ground truth, endpoint counts
	// and the spec diff are all computed before anything statistical happens,
	// and they are what a reader checks when the numbers look wrong.
	if cfg.asJSON {
		if err := eval.WriteJSON(stdout, res); err != nil {
			return err
		}
	} else if err := eval.WriteSummary(stdout, res); err != nil {
		return err
	}

	if !cfg.skipLatency {
		if err := runLatency(cfg, stdout); err != nil {
			return errors.Join(runErr, err)
		}
	}
	return runErr
}

// runLatency performs the sweep and writes its summary and, if asked, its CSV.
func runLatency(cfg evalConfig, stdout io.Writer) error {
	sweep := eval.DefaultLatencyConfig()
	sweep.Seed = cfg.seed
	sweep.Trials = cfg.latencyTrials

	points, err := eval.LatencySweep(sweep)
	if err != nil {
		return err
	}

	if cfg.latencyCSV != "" {
		f, err := os.Create(cfg.latencyCSV)
		if err != nil {
			return fmt.Errorf("creating %s: %w", cfg.latencyCSV, err)
		}
		defer f.Close()

		if err := eval.WriteLatencyCSV(f, points); err != nil {
			return fmt.Errorf("writing %s: %w", cfg.latencyCSV, err)
		}
		fmt.Fprintf(stdout, "\nwrote the detection-latency grid to %s\n", cfg.latencyCSV)
	}

	fmt.Fprintln(stdout)
	return eval.WriteLatencySummary(stdout, eval.SummarizeLatency(points, cfg.latencyThreshold), cfg.latencyThreshold)
}
