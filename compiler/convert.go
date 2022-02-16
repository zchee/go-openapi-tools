// Copyright 2022 The go-openapi-tools Authors.
// SPDX-License-Identifier: BSD-3-Clause

package compiler

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

// IsDigit returns whether the r is digit.
func IsDigit(c byte) bool {
	return '0' <= c && c <= '9'
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

	if IsDigit(s[0]) {
		s = "X_" + s
	}

	return s
}
