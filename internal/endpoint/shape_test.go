package endpoint

import "testing"

func TestIsIDShaped(t *testing.T) {
	tests := []struct {
		name string
		seg  string
		want bool
	}{
		// Numeric database ids.
		{"short numeric id", "8", true},
		{"numeric id", "8123", true},
		{"long numeric id", "99447326518", true},

		// UUIDs, in both cases, and near misses.
		{"lowercase uuid", "3f2504e0-4f89-11d3-9a0c-0305e82c3301", true},
		{"uppercase uuid", "3F2504E0-4F89-11D3-9A0C-0305E82C3301", true},
		{"uuid with a wrong separator", "3f2504e0_4f89_11d3_9a0c_0305e82c3301", false},
		{"uuid missing a character", "3f2504e0-4f89-11d3-9a0c-0305e82c330", false},
		{"uuid with a non-hex character", "3g2504e0-4f89-11d3-9a0c-0305e82c3301", false},

		// Hex digests.
		{"truncated sha", "a94a8fe5ccb1", true},
		{"full sha1", "a94a8fe5ccb19ba61c4c0873d391e987982fbbd3", true},
		{"hex too short to be a digest", "abc123", false},
		{"a hex-spellable word stays a route", "facade", false},
		{"decade is a route, not a digest", "decade", false},

		// Prefixed opaque ids, the Stripe convention.
		{"stripe customer id", "cus_QxYzABCDEFgh", true},
		{"stripe charge id", "ch_3PxYzABCDEFghijk0LmNoPqR", true},
		{"snake_case route name is not an id", "payment_intents", false},
		{"snake_case route with no digits", "billing_portal_sessions", false},
		{"prefix too long to be a prefix", "verylongprefix_ABCDEFGH1", false},
		{"body too short after the prefix", "cus_Ab1", false},
		{"leading underscore", "_abcdefgh1", false},

		// Long opaque tokens.
		{"ulid", "01ARZ3NDEKTSV4RRFFQ69G5FAV", true},
		{"long token with no digit is not opaque enough", "abcdefghijklmnopqrstuvwxyz", false},
		{"hyphenated long name stays a route", "some-very-long-route-name-here", false},

		// Ordinary route names.
		{"short word", "docs", false},
		{"another word", "webhooks", false},
		{"pricing", "pricing", false},
		{"auth", "auth", false},
		{"version segment", "v1", false},
		{"hyphenated route", "payment-intents", false},
		{"filename", "openapi.json", false},
		{"empty segment", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isIDShaped(tt.seg); got != tt.want {
				t.Errorf("isIDShaped(%q) = %v, want %v", tt.seg, got, tt.want)
			}
		})
	}
}
