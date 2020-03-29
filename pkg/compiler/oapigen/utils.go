// Copyright 2020 The go-openapi-tools Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package oapigen

import (
	"sort"
	"strings"
	"unicode"
)

// NormalizeParam normalizes param for hack for invalid spinnaker swagger schema.
func NormalizeParam(param string) string {
	s := param
	if idx := strings.LastIndex(param, "]"); idx > -1 {
		s = param[idx+1:]
	}

	if first := rune(s[0]); unicode.IsUpper(first) {
		s = string(unicode.ToLower(first)) + s[1:]
	}

	return s
}

// IsVowel returns whether the r is vowel letter.
func IsVowel(r rune) bool {
	switch r {
	case 'a', 'e', 'i', 'o', 'u':
		return true
	default:
		return false
	}
}

// SortedMapKeys returns the keys of m, which must be a map[string]T, in sorted order.
func SortedMapKeys(v interface{}) []string {
	m, ok := v.(map[string]interface{})
	if !ok {
		return nil
	}

	keys := make([]string, len(m))
	i := 0
	for k := range m {
		keys[i] = k
		i++
	}
	sort.Strings(keys)

	return keys
}

// Depunct removes '-', '.', '$', '/', '_' from identifers, making the
// following character uppercase. Multiple '_' are preserved.
func Depunct(ident string, needCap bool) string {
	var sb strings.Builder

	preserve := false
	for i, c := range ident {
		if c == '_' {
			if preserve || strings.HasPrefix(ident[i:], "__") {
				preserve = true
			} else {
				needCap = true
				continue
			}
		} else {
			preserve = false
		}
		if c == '-' || c == '.' || c == '$' || c == '/' {
			needCap = true
			continue
		}
		if needCap {
			c = unicode.ToUpper(c)
			needCap = false
		}
		sb.WriteByte(byte(c))
	}

	s := FixName(sb.String())
	if s == "Typ" {
		return "Type" // special case
	}
	return s
}

// commonInitialisms is a set of common initialisms.
// Only add entries that are highly unlikely to be non-initialisms.
// For instance, "ID" is fine (Freudian code is rare), but "AND" is not.
//
// ported from golang.org/x/lint/lint.go
var commonInitialisms = map[string]bool{
	"ACL":   true,
	"API":   true,
	"ASCII": true,
	"CPU":   true,
	"CSS":   true,
	"DNS":   true,
	"EOF":   true,
	"GUID":  true,
	"HTML":  true,
	"HTTP":  true,
	"HTTPS": true,
	"ID":    true,
	"IP":    true,
	"JSON":  true,
	"LHS":   true,
	"QPS":   true,
	"RAM":   true,
	"RHS":   true,
	"RPC":   true,
	"SLA":   true,
	"SMTP":  true,
	"SQL":   true,
	"SSH":   true,
	"TCP":   true,
	"TLS":   true,
	"TTL":   true,
	"UDP":   true,
	"UI":    true,
	"UID":   true,
	"UUID":  true,
	"URI":   true,
	"URL":   true,
	"UTF8":  true,
	"VM":    true,
	"XML":   true,
	"XMPP":  true,
	"XSRF":  true,
	"XSS":   true,
}

// FixName returns a fixed name based by Go naming idiom.
//
// ported from golang.org/x/lint/lint.go
func FixName(name string) (should string) {
	// Fast path for simple cases: "_" and all lowercase.
	if name == "_" {
		return name
	}

	if name == "type" {
		return "typ"
	}

	allLower := true
	for _, r := range name {
		if !unicode.IsLower(r) {
			allLower = false
			break
		}
	}
	if allLower {
		return name
	}

	// Split camelCase at any lower->upper transition, and split on underscores.
	// Check each word for common initialisms.
	runes := []rune(name)
	w, i := 0, 0 // index of start of word, scan
	for i+1 <= len(runes) {
		eow := false // whether we hit the end of a word

		switch {
		case i+1 == len(runes):
			eow = true

		case runes[i+1] == '_':
			// underscore; shift the remainder forward over any run of underscores
			eow = true
			n := 1
			for i+n+1 < len(runes) && runes[i+n+1] == '_' {
				n++
			}

			// Leave at most one underscore if the underscore is between two digits
			if i+n+1 < len(runes) && unicode.IsDigit(runes[i]) && unicode.IsDigit(runes[i+n+1]) {
				n--
			}

			copy(runes[i+1:], runes[i+n+1:])
			runes = runes[:len(runes)-n]

		case unicode.IsLower(runes[i]) && !unicode.IsLower(runes[i+1]):
			// lower->non-lower
			eow = true
		}
		i++
		if !eow {
			continue
		}

		// [w,i) is a word.
		word := string(runes[w:i])
		if u := strings.ToUpper(word); commonInitialisms[u] {
			// Keep consistent case, which is lowercase only at the start.
			if w == 0 && unicode.IsLower(runes[w]) {
				u = strings.ToLower(u)
			}
			// All the common initialisms are ASCII,
			// so we can replace the bytes exactly.
			copy(runes[w:], []rune(u))
		} else if w > 0 && strings.ToLower(word) == word {
			// already all lowercase, and not the first word, so uppercase the first character.
			runes[w] = unicode.ToUpper(runes[w])
		}
		w = i
	}
	return string(runes)
}
