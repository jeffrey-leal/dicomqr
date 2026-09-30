package main

import (
	"strings"
	"testing"
)

// The tag picker now filters on precomputed, NUL-joined search keys. For every
// dictionary entry and a spread of queries — name and keyword fragments, the
// comma and run-together number forms, a bare group, and one that matches
// nothing — the key must match exactly when tagMatchesQuery does.
func TestDictionarySearchKeysMatchTagMatchesQuery(t *testing.T) {
	infos, keys := dictionaryTags(), dictionarySearchKeys()
	if len(infos) != len(keys) {
		t.Fatalf("%d keys for %d dictionary entries", len(keys), len(infos))
	}
	for _, q := range []string{"patient", "patientname", "0010,0010", "00100010", "0010", "7fe0", "name", "zzzz"} {
		for i, info := range infos {
			want := tagMatchesQuery(info, q)
			got := strings.Contains(keys[i], q)
			if got != want {
				t.Fatalf("query %q, tag %s (%s): key match %v, tagMatchesQuery %v", q, formatTagRef(info.Tag), info.Name, got, want)
			}
		}
	}
}
