package detect

import (
	"time"

	"github.com/avidnerd/apidrift/internal/endpoint"
	"github.com/avidnerd/apidrift/internal/schema"
)

// ChangeKind is what happened to a field between two windows.
type ChangeKind uint8

const (
	// FieldRemoved is a field whose presence rate collapsed.
	FieldRemoved ChangeKind = iota
	// TypeChanged is a field that began arriving as a different JSON type.
	TypeChanged
	// NullabilityIntroduced is a field that began arriving as explicit null.
	NullabilityIntroduced
	// EnumValueAdded is a string field that began taking a value never seen
	// in the baseline.
	EnumValueAdded
	// FieldAdded is a path that did not exist in the baseline.
	FieldAdded
)

// String renders a ChangeKind for reports.
func (c ChangeKind) String() string {
	switch c {
	case FieldRemoved:
		return "field_removed"
	case TypeChanged:
		return "type_changed"
	case NullabilityIntroduced:
		return "nullability_introduced"
	case EnumValueAdded:
		return "enum_value_added"
	case FieldAdded:
		return "field_added"
	}
	return "unknown"
}

// MarshalText renders the kind as its name, so JSON output carries
// "type_changed" rather than 1 and stays readable when the constants move.
func (c ChangeKind) MarshalText() ([]byte, error) { return []byte(c.String()), nil }

// UnmarshalText parses the name form written by MarshalText.
func (c *ChangeKind) UnmarshalText(b []byte) error {
	for _, k := range AllChangeKinds {
		if k.String() == string(b) {
			*c = k
			return nil
		}
	}
	return &parseError{kind: "detect.ChangeKind", value: string(b)}
}

// AllChangeKinds lists every kind, for iteration in reports and scoring.
var AllChangeKinds = []ChangeKind{
	FieldRemoved, TypeChanged, NullabilityIntroduced, EnumValueAdded, FieldAdded,
}

// Severity is how much a finding should worry the reader.
type Severity uint8

const (
	// SeverityInfo is a change that is unlikely to break a caller.
	SeverityInfo Severity = iota
	// SeverityHigh is a change that will break a caller that relied on the
	// field.
	SeverityHigh
	// SeverityCritical is a change that breaks parsing or silently corrupts
	// values, rather than merely failing a lookup.
	SeverityCritical
)

// String renders a Severity for reports.
func (s Severity) String() string {
	switch s {
	case SeverityInfo:
		return "info"
	case SeverityHigh:
		return "high"
	case SeverityCritical:
		return "critical"
	}
	return "unknown"
}

// MarshalText renders the severity as its name.
func (s Severity) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// UnmarshalText parses the name form written by MarshalText.
func (s *Severity) UnmarshalText(b []byte) error {
	for _, v := range []Severity{SeverityInfo, SeverityHigh, SeverityCritical} {
		if v.String() == string(b) {
			*s = v
			return nil
		}
	}
	return &parseError{kind: "detect.Severity", value: string(b)}
}

// parseError reports an unrecognised name in JSON input.
type parseError struct{ kind, value string }

func (e *parseError) Error() string { return "unknown " + e.kind + ": " + e.value }

// Window is one side of a comparison: a schema and the period it covers.
type Window struct {
	From, To time.Time
	Schema   *schema.Schema
}

// Config tunes the detector.
type Config struct {
	MinSamples     uint64  // per window, below this we don't test
	MinEffectSize  float64 // absolute change in presence rate, e.g. 0.10
	FDRAlpha       float64 // Benjamini-Hochberg target, e.g. 0.05
	PersistWindows int     // consecutive windows a finding must survive
}

// DefaultConfig returns the tuning the project ships with.
func DefaultConfig() Config {
	return Config{
		MinSamples:     200,
		MinEffectSize:  0.10,
		FDRAlpha:       0.05,
		PersistWindows: 2,
	}
}

// withDefaults fills zero fields, so callers can override one knob.
func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.MinSamples == 0 {
		c.MinSamples = d.MinSamples
	}
	if c.MinEffectSize == 0 {
		c.MinEffectSize = d.MinEffectSize
	}
	if c.FDRAlpha == 0 {
		c.FDRAlpha = d.FDRAlpha
	}
	if c.PersistWindows == 0 {
		c.PersistWindows = d.PersistWindows
	}
	return c
}

// Finding is one reported change.
type Finding struct {
	// Endpoint is the endpoint the change was found on. Detect does not know
	// it -- its inputs are two windows and nothing else -- so Detect leaves it
	// zero and the caller stamps it. See README.md.
	Endpoint endpoint.Key `json:"endpoint"`
	// Path is the JSON path that changed.
	Path string `json:"path"`
	// Kind is what changed.
	Kind ChangeKind `json:"kind"`
	// Severity is how much it matters.
	Severity Severity `json:"severity"`
	// BaselineRate and CurrentRate are the field's presence rates in the two
	// windows, or for a type change, the rate of the kind in question.
	BaselineRate float64 `json:"baseline_rate"`
	CurrentRate  float64 `json:"current_rate"`
	// PValue is the significance of the difference before correction.
	PValue float64 `json:"p_value"`
	// Evidence is a short human-readable line carrying the raw counts, e.g.
	// "present in 2000/2000 baseline, 94/100 current".
	Evidence string `json:"evidence"`
}

// ID identifies a finding across windows: the same change reported again in a
// later window has the same ID. Rates and p-values move from window to window
// and so are deliberately not part of it.
type ID struct {
	Endpoint endpoint.Key
	Path     string
	Kind     ChangeKind
}

// ID returns the finding's cross-window identity.
func (f Finding) ID() ID {
	return ID{Endpoint: f.Endpoint, Path: f.Path, Kind: f.Kind}
}
