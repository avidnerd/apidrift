package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseFixFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		check   func(*testing.T, fixConfig)
		wantErr string
	}{
		{
			name: "writes nothing by default",
			args: nil,
			check: func(t *testing.T, c fixConfig) {
				if c.apply || c.openPR {
					t.Error("apply/pr default to true; the safe path should be the default one")
				}
				if !strings.HasPrefix(c.branch, "apidrift/fix-") {
					t.Errorf("branch = %q, want a generated apidrift/fix- name", c.branch)
				}
			},
		},
		{
			name: "-pr implies -apply",
			args: []string{"-pr"},
			check: func(t *testing.T, c fixConfig) {
				if !c.apply {
					t.Error("-pr did not imply -apply; it would open an empty pull request")
				}
			},
		},
		{
			name:    "zero max is rejected",
			args:    []string{"-max", "0"},
			wantErr: "at least 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseFixFlags(tt.args, &bytes.Buffer{})
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

func TestFixWithNoProxyRunning(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"fix", "-admin", "http://127.0.0.1:1"}, &out, &errOut); code == exitOK {
		t.Error("fix against a dead proxy exited 0")
	}
	if !strings.Contains(errOut.String(), "apidrift demo -serve") {
		t.Errorf("error does not say what to start:\n%s", errOut.String())
	}
}
