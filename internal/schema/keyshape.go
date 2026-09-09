package schema

import "strings"

// Object-key shape rules, used only to tell a map's keys from a record's field
// names.
//
// These are deliberately not shared with the segment rules in package endpoint,
// despite the obvious family resemblance. The two answer different questions
// against different data: a path segment is a URL component and the common
// forms are bare identifiers, while a map key is usually a *qualified* one --
// "user_8123", "acct_9944" -- where the qualifying prefix is the norm rather
// than a special case. Sharing one rule set would mean one set of thresholds
// serving neither well, and every later tuning change to one would silently
// retune the other.
const (
	// keyMinHexLen is the length at which an all-hex key is a digest, matching
	// the reasoning in package endpoint: long enough to exclude hex-spellable
	// English words.
	keyMinHexLen = 12
	// keyMinOpaqueLen is the length at which an unbroken alphanumeric key is an
	// opaque token rather than a name.
	keyMinOpaqueLen = 20
	// keyMaxPrefixLen bounds the qualifying prefix in the "user_8123" form.
	keyMaxPrefixLen = 12
	// keyMinAlnumSuffixLen is the shortest non-numeric body accepted after a
	// prefix. A numeric body needs no minimum: "user_8" is unambiguous.
	keyMinAlnumSuffixLen = 8
)

// isKeyIDShaped reports whether an object key looks like data rather than a
// hand-written field name.
func isKeyIDShaped(key string) bool {
	if key == "" {
		return false
	}
	switch {
	case allDigits(key):
		return true
	case isUUIDKey(key):
		return true
	case len(key) >= keyMinHexLen && allHex(key):
		return true
	case isQualifiedIDKey(key):
		return true
	case len(key) >= keyMinOpaqueLen && isOpaqueKey(key):
		return true
	}
	return false
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func hexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func allHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if !hexDigit(s[i]) {
			return false
		}
	}
	return true
}

// isUUIDKey recognises the canonical 8-4-4-4-12 hyphenated form.
func isUUIDKey(s string) bool {
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
		if !hexDigit(s[i]) {
			return false
		}
	}
	return true
}

// isQualifiedIDKey recognises "user_8123" and "cus_QxYzABCDEFgh": a short
// lowercase prefix, one underscore, then either a number or a long token.
//
// The numeric case has no length minimum, which is what separates this from the
// segment rules: "user_8" is plainly a key built from an id, whereas a bare "8"
// as a path segment needed no such qualification to be recognised.
func isQualifiedIDKey(s string) bool {
	i := strings.IndexByte(s, '_')
	if i <= 0 || i > keyMaxPrefixLen {
		return false
	}
	prefix, suffix := s[:i], s[i+1:]
	if suffix == "" {
		return false
	}
	for j := 0; j < len(prefix); j++ {
		c := prefix[j]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	if allDigits(suffix) {
		return true
	}
	return len(suffix) >= keyMinAlnumSuffixLen && isOpaqueKey(suffix)
}

// isOpaqueKey recognises an unbroken alphanumeric run carrying a digit or mixed
// case: a ULID, a base62 key, a hash fragment. Field names are lowercase words,
// so neither trait appears in one.
func isOpaqueKey(s string) bool {
	var hasDigit, hasUpper, hasLower bool
	for i := 0; i < len(s); i++ {
		c := s[i]
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
