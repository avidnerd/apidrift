package eval

import (
	"encoding/csv"
	"fmt"
	"io"
	"math/rand"
	"sort"
	"time"

	"github.com/avidnerd/apidrift/internal/detect"
	"github.com/avidnerd/apidrift/internal/schema"
)

// LatencyConfig configures the detection-latency sweep.
//
// The sweep answers: how many responses does apidrift need before it notices a
// field's presence rate has dropped to p? A field that vanishes outright is
// obvious within a handful; one that drops from 100% to 90% takes hundreds,
// because at small n that difference is indistinguishable from luck. That curve
// is the honest statement of what the tool can and cannot see, and it is a
// property of the statistics rather than of any implementation detail.
type LatencyConfig struct {
	// Path is the JSON path whose presence rate is varied.
	Path string
	// BaselineSamples and BaselineRate describe the baseline window, held
	// fixed across the sweep.
	BaselineSamples uint64
	BaselineRate    float64
	// Rates are the current-window presence rates to sweep.
	Rates []float64
	// Steps are the current-window sample counts to try, ascending.
	Steps []uint64
	// Trials is how many independent draws are made per (rate, step) cell.
	// One draw would report the luck of one seed as if it were the answer.
	Trials int
	// Detect configures the detector.
	Detect detect.Config
	// Seed makes the sweep reproducible.
	Seed int64
}

// DefaultLatencyConfig returns a sweep that covers the interesting range: a
// total disappearance, and drops small enough to be genuinely hard.
func DefaultLatencyConfig() LatencyConfig {
	return LatencyConfig{
		Path:            "status",
		BaselineSamples: 5000,
		BaselineRate:    1.0,
		// Rates cluster around the effect-size gate, because that is where the
		// interesting behaviour is: above it detection is immediate once the
		// sample floor is cleared, and below it nothing is ever detected at
		// any sample size. The sweep exists to locate that cliff precisely.
		Rates:  []float64{0.0, 0.25, 0.50, 0.70, 0.80, 0.85, 0.88, 0.90, 0.91, 0.95},
		Steps:  []uint64{50, 100, 150, 200, 300, 500, 1000, 2000, 4000},
		Trials: 40,
		Detect: detect.DefaultConfig(),
		Seed:   1,
	}
}

// LatencyPoint is one cell of the sweep: how often a drop to PresenceRate was
// detected from Responses responses.
type LatencyPoint struct {
	PresenceRate float64 `json:"presence_rate"`
	Responses    uint64  `json:"responses"`
	Trials       int     `json:"trials"`
	Detections   int     `json:"detections"`
}

// DetectionProbability is the share of trials in which a finding fired.
func (p LatencyPoint) DetectionProbability() float64 {
	if p.Trials == 0 {
		return 0
	}
	return float64(p.Detections) / float64(p.Trials)
}

// LatencySummary is the headline per rate: the fewest responses at which
// detection became more likely than not.
type LatencySummary struct {
	PresenceRate float64 `json:"presence_rate"`
	// ResponsesRequired is the smallest step whose detection probability
	// reached the threshold.
	ResponsesRequired uint64 `json:"responses_required"`
	// Reached reports whether any step did. False means the drop was too small
	// to find within the sweep's largest step.
	Reached bool `json:"reached"`
}

// LatencySweep measures detection latency across the configured grid.
//
// The windows are constructed directly from binomial draws rather than by
// generating and merging response bodies. That is deliberate: it isolates the
// detector's statistical power, which is what the question is about, and it
// keeps the measurement independent of how fast extraction happens to be. It
// also means the sweep needs only detect.Detect, not schema.Merge.
func LatencySweep(cfg LatencyConfig) ([]LatencyPoint, error) {
	if cfg.Trials <= 0 {
		cfg.Trials = 1
	}
	if cfg.Path == "" {
		cfg.Path = DefaultLatencyConfig().Path
	}

	rng := rand.New(rand.NewSource(cfg.Seed))
	now := time.Unix(0, 0).UTC()

	var points []LatencyPoint
	for _, rate := range cfg.Rates {
		for _, step := range cfg.Steps {
			point := LatencyPoint{PresenceRate: rate, Responses: step, Trials: cfg.Trials}

			for i := 0; i < cfg.Trials; i++ {
				baseline := oneFieldWindow(cfg.Path, cfg.BaselineSamples, binomial(rng, cfg.BaselineSamples, cfg.BaselineRate))
				current := oneFieldWindow(cfg.Path, step, binomial(rng, step, rate))

				found, err := safeDetect(
					detect.Window{From: now.Add(-2 * time.Hour), To: now.Add(-time.Hour), Schema: baseline},
					detect.Window{From: now.Add(-time.Hour), To: now, Schema: current},
					cfg.Detect,
				)
				if err != nil {
					return nil, err
				}
				for _, f := range found {
					if f.Path == cfg.Path {
						point.Detections++
						break
					}
				}
			}
			points = append(points, point)
		}
	}
	return points, nil
}

// SummarizeLatency reduces the grid to the fewest responses at which each rate
// crossed the detection threshold.
func SummarizeLatency(points []LatencyPoint, threshold float64) []LatencySummary {
	byRate := make(map[float64][]LatencyPoint)
	for _, p := range points {
		byRate[p.PresenceRate] = append(byRate[p.PresenceRate], p)
	}

	out := make([]LatencySummary, 0, len(byRate))
	for rate, ps := range byRate {
		sort.Slice(ps, func(i, j int) bool { return ps[i].Responses < ps[j].Responses })

		summary := LatencySummary{PresenceRate: rate}
		for _, p := range ps {
			if p.DetectionProbability() >= threshold {
				summary.ResponsesRequired, summary.Reached = p.Responses, true
				break
			}
		}
		out = append(out, summary)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PresenceRate < out[j].PresenceRate })
	return out
}

// WriteLatencyCSV writes the full grid, one row per cell, for plotting.
func WriteLatencyCSV(w io.Writer, points []LatencyPoint) error {
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"presence_rate", "responses", "trials", "detections", "detection_probability"}); err != nil {
		return err
	}
	for _, p := range points {
		row := []string{
			fmt.Sprintf("%g", p.PresenceRate),
			fmt.Sprintf("%d", p.Responses),
			fmt.Sprintf("%d", p.Trials),
			fmt.Sprintf("%d", p.Detections),
			fmt.Sprintf("%.4f", p.DetectionProbability()),
		}
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// oneFieldWindow builds a schema describing one field observed in `present` of
// `seen` responses.
func oneFieldWindow(path string, seen, present uint64) *schema.Schema {
	return &schema.Schema{
		Samples: seen,
		Fields: map[string]*schema.FieldStats{
			path: {
				Seen:         seen,
				Present:      present,
				TypeCounts:   map[schema.Kind]uint64{schema.KindString: present},
				StringValues: map[string]uint64{"open": present},
			},
		},
	}
}

// binomial draws the number of successes in n trials at probability p.
func binomial(rng *rand.Rand, n uint64, p float64) uint64 {
	var k uint64
	for i := uint64(0); i < n; i++ {
		if rng.Float64() < p {
			k++
		}
	}
	return k
}
