package endpoint

import "strings"

// Shape rules recognise a path segment as an identifier from its form alone,
// without waiting for statistics. This is the fast tier of the heuristic: an
// identifier is recognisable on sight, and traffic that is entirely
// identifiers -- most of it -- never has to accumulate evidence.
//
// The rules are deliberately conservative. A false positive here permanently
// merges two distinct static routes into one endpoint, hiding real drift
// between them; a false negative merely means the statistical tier has to do
// the work instead, a few hundred requests later. So each rule requires a form
// that a hand-written route segment would essentially never take.
const (
	// minHexLen is the length at which an all-hex segment is taken to be a
	// digest or object id rather than a word. Twelve excludes English words
	// spellable in hex ("added", "decade", "efface", "facade") with room to
	// spare, and admits truncated SHAs, which are conventionally 12 or more.
	minHexLen = 12

	// minOpaqueLen is the length at which a mixed alphanumeric segment with no
	// word structure is taken to be an opaque token: ULIDs are 26, and base62
	// identifiers are rarely shorter than 20. Route names this long exist but
	// are hyphenated or underscored, which disqualifies them.
	minOpaqueLen = 20

	// minSuffixLen is the shortest identifier body accepted after a prefix in
	// the "cus_QxYz..." convention Stripe and others use.
	minSuffixLen = 8
	// maxPrefixLen bounds the prefix in that convention, so a long snake_case
	// route name is not mistaken for a prefixed identifier.
	maxPrefixLen = 10
)

// isIDShaped reports whether a path segment looks like an identifier.
func isIDShaped(seg string) bool {
	if seg == "" {
		return false
	}
	switch {
	case isAllDigits(seg):
		// Numeric segments are database ids in overwhelming preponderance.
		// The counterexample -- a route literally named "2024" -- exists, but
		// it is rare enough to be worth the exchange.
		return true
	case isUUID(seg):
		return true
	case len(seg) >= minHexLen && isAllHex(seg):
		return true
	case isPrefixedID(seg):
		return true
	case len(seg) >= minOpaqueLen && isOpaqueToken(seg):
		return true
	}
	return false
}

func isAllDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func isAllHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if !isHexDigit(s[i]) {
			return false
		}
	}
	return true
}

// isUUID recognises the canonical 8-4-4-4-12 hyphenated form, case-insensitive.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if s[i] != '-' {
				return false
			}
			continue
		}
		if !isHexDigit(s[i]) {
			return false
		}
	}
	return true
}

// isPrefixedID recognises the "cus_QxYzAbCd" convention: a short lowercase
// prefix, one underscore, then a long alphanumeric body.
func isPrefixedID(s string) bool {
	i := strings.IndexByte(s, '_')
	if i <= 0 || i > maxPrefixLen {
		return false
	}
	prefix, suffix := s[:i], s[i+1:]
	if len(suffix) < minSuffixLen {
		return false
	}
	for j := 0; j < len(prefix); j++ {
		if prefix[j] < 'a' || prefix[j] > 'z' {
			return false
		}
	}
	// The body must be alphanumeric, and must carry a digit or mixed case.
	// That is what separates "cus_QxYzABCDEFgh" from a snake_case route name
	// like "payment_intents": route names are conventionally lowercase words,
	// and a generated identifier almost never is.
	var hasDigit, hasUpper, hasLower bool
	for j := 0; j < len(suffix); j++ {
		c := suffix[j]
		switch {
		case c >= '0' && c <= '9':
			hasDigit = true
		case c >= 'a' && c <= 'z':
			hasLower = true
		case c >= 'A' && c <= 'Z':
			hasUpper = true
		default:
			return false
		}
	}
	return hasDigit || (hasUpper && hasLower)
}

// isOpaqueToken recognises a long unbroken alphanumeric run containing a digit:
// a ULID, a base62 key, a session token. The digit requirement and the absence
// of any separator are what keep long route names out.
func isOpaqueToken(s string) bool {
	hasDigit := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			hasDigit = true
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
		default:
			return false
		}
	}
	return hasDigit
}
