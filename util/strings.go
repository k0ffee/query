//  Copyright 2014-Present Couchbase, Inc.
//
//  Use of this software is governed by the Business Source License included
//  in the file licenses/BSL-Couchbase.txt.  As of the Change Date specified
//  in that file, in accordance with the Business Source License, use of this
//  software will be governed by the Apache License, Version 2.0, included in
//  the file licenses/APL2.txt.

package util

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

/*
Note: The input slices must be sorted beforehand.
*/
func SortedStringsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	if a == nil || b == nil {
		return a == nil && b == nil
	}

	for i, _ := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

/*
This code copied verbatim from
http://rosettacode.org/wiki/Reverse_a_string#Go. With gratitude to
rosettacode.org and the author(s).
*/

// reversePreservingCombiningCharacters interprets its argument as UTF-8
// and ignores bytes that do not form valid UTF-8.  return value is UTF-8.
func ReversePreservingCombiningCharacters(s string) string {
	if s == "" {
		return ""
	}
	p := []rune(s)
	r := make([]rune, len(p))
	start := len(r)
	for i := 0; i < len(p); {
		// quietly skip invalid UTF-8
		if p[i] == utf8.RuneError {
			i++
			continue
		}
		j := i + 1
		for j < len(p) && (unicode.Is(unicode.Mn, p[j]) ||
			unicode.Is(unicode.Me, p[j]) || unicode.Is(unicode.Mc, p[j])) {
			j++
		}
		for k := j - 1; k >= i; k-- {
			start--
			r[start] = p[k]
		}
		i = j
	}
	return (string(r[start:]))
}

func TrimSpace(s string) string {

	if len(s) > 0 {
		if s[0] == ' ' || s[len(s)-1] == ' ' {
			s = strings.TrimSpace(s)
		}
	}

	return s
}
