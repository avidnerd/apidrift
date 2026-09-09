package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunDispatch(t *testing.T) {
	tests := []struct {
		name         string
		args         []string
		wantCode     int
		wantContains string
		wantStream   string // "out" or "err"
	}{
		{"no arguments prints usage", nil, exitUsage, "apidrift serve", "err"},
		{"help goes to stdout", []string{"help"}, exitOK, "apidrift report", "out"},
		{"-h is help", []string{"-h"}, exitOK, "usage:", "out"},
		{"--help is help", []string{"--help"}, exitOK, "usage:", "out"},
		{"an unknown command is a usage error", []string{"wibble"}, exitUsage, `unknown command "wibble"`, "err"},
		{"serve without an upstream is a usage error", []string{"serve"}, exitUsage, "requires -upstream", "err"},
		{"serve -h exits cleanly", []string{"serve", "-h"}, exitOK, "-upstream", "err"},
		{"report -h exits cleanly", []string{"report", "-h"}, exitOK, "-admin", "err"},
		{"an unknown flag is a usage error", []string{"serve", "-nonsense"}, exitUsage, "", "err"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			got := run(tt.args, &out, &errOut)

			if got != tt.wantCode {
				t.Errorf("run(%v) = %d, want %d\nstdout: %s\nstderr: %s", tt.args, got, tt.wantCode, out.String(), errOut.String())
			}
			if tt.wantContains == "" {
				return
			}
			stream := out.String()
			if tt.wantStream == "err" {
				stream = errOut.String()
			}
			if !strings.Contains(stream, tt.wantContains) {
				t.Errorf("%s does not contain %q:\n%s", tt.wantStream, tt.wantContains, stream)
			}
		})
	}
}

func TestParseServeFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
		check   func(*testing.T, serveConfig)
	}{
		{
			name: "defaults",
			args: []string{"-upstream", "https://api.stripe.com"},
			check: func(t *testing.T, c serveConfig) {
				if c.listen != ":8080" {
					t.Errorf("listen = %q, want :8080", c.listen)
				}
				if c.admin != "127.0.0.1:9090" {
					t.Errorf("admin = %q, want 127.0.0.1:9090 -- the admin surface should not be public by default", c.admin)
				}
			},
		},
		{
			name:    "upstream is required",
			args:    nil,
			wantErr: "requires -upstream",
		},
		{
			name:    "a zero window is rejected",
			args:    []string{"-upstream", "http://x", "-window", "0"},
			wantErr: "must be positive",
		},
		{
			name:    "a negative baseline is rejected",
			args:    []string{"-upstream", "http://x", "-baseline", "-1h"},
			wantErr: "must be positive",
		},
		{
			name:    "a zero queue is rejected",
			args:    []string{"-upstream", "http://x", "-queue", "0"},
			wantErr: "at least 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseServeFlags(tt.args, &bytes.Buffer{})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}

func TestServeRejectsABadUpstream(t *testing.T) {
	var out, errOut bytes.Buffer
	if got := run([]string{"serve", "-upstream", "ftp://example.test"}, &out, &errOut); got != exitUsage {
		t.Errorf("run() = %d, want %d for a non-http upstream", got, exitUsage)
	}
	if !strings.Contains(errOut.String(), "scheme must be http or https") {
		t.Errorf("stderr does not explain the problem:\n%s", errOut.String())
	}
}

func TestBucketFor(t *testing.T) {
	tests := []struct {
		window string
		want   string
	}{
		{"30s", "1m0s"},
		{"5m", "1m0s"},
		{"59m", "1m0s"},
		{"1h", "1h0m0s"},
		{"24h", "1h0m0s"},
	}
	for _, tt := range tests {
		t.Run(tt.window, func(t *testing.T) {
			d, err := parseDuration(tt.window)
			if err != nil {
				t.Fatalf("parsing %q: %v", tt.window, err)
			}
			if got := bucketFor(d).String(); got != tt.want {
				t.Errorf("bucketFor(%s) = %s, want %s", tt.window, got, tt.want)
			}
		})
	}
}
