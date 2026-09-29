package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestValidateSOPClassUIDs(t *testing.T) {
	got, err := validateSOPClassUIDs([]string{" 1.2.840.10008.5.1.4.1.1.7 ", "", "1.2.840.10008.5.1.4.1.1.104.1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"1.2.840.10008.5.1.4.1.1.7", "1.2.840.10008.5.1.4.1.1.104.1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	// Nothing left must be nil, not an empty slice: Preferences' DeepEqual
	// change detection would otherwise rewrite profiles.json on every Apply.
	if got, err := validateSOPClassUIDs([]string{"", "  "}); err != nil || got != nil {
		t.Errorf("blank list: got %#v, %v; want nil, nil", got, err)
	}

	for _, bad := range []string{"SC", "1.2.840.10008.5.1.4.1.1.7x", "1..2", ".1.2", "1.2.", strings.Repeat("1", 65)} {
		if _, err := validateSOPClassUIDs([]string{"1.2.840.10008.5.1.4.1.1.7", bad}); err == nil {
			t.Errorf("%q accepted as a UID", bad)
		} else if !strings.Contains(err.Error(), bad) {
			t.Errorf("error for %q does not name it: %v", bad, err)
		}
	}
}

func TestSOPClassNaming(t *testing.T) {
	if name := sopClassName("1.2.840.10008.5.1.4.1.1.7"); name != "Secondary Capture Image" {
		t.Errorf("sopClassName(SC) = %q", name)
	}
	if name := sopClassName("1.2.3.4"); name != "" {
		t.Errorf("unknown UID named %q", name)
	}
	if got := describeSOPClass("1.2.840.10008.5.1.4.1.1.104.1"); got != "Encapsulated PDF (1.2.840.10008.5.1.4.1.1.104.1)" {
		t.Errorf("describeSOPClass = %q", got)
	}
	if got := describeSOPClass("1.2.3.4"); got != "1.2.3.4" {
		t.Errorf("describeSOPClass(unknown) = %q", got)
	}
	summary := sopClassListSummary([]string{"1.2.840.10008.5.1.4.1.1.7", "", "1.2.3.4"})
	if summary != "Secondary Capture Image; 1.2.3.4" {
		t.Errorf("sopClassListSummary = %q", summary)
	}
	if sopClassListSummary(nil) != "" {
		t.Error("empty summary must be empty")
	}
	// Every member of the recommended family is named, so the hint line
	// under the editor field reads as names rather than as five UIDs.
	for _, uid := range secondaryCaptureSOPClasses {
		if sopClassName(uid) == "" {
			t.Errorf("secondary capture class %s has no name", uid)
		}
	}
}

func TestMatchesSOPClass(t *testing.T) {
	list := []string{"1.2.840.10008.5.1.4.1.1.7"}
	if !matchesSOPClass("1.2.840.10008.5.1.4.1.1.7", list) {
		t.Error("exact match failed")
	}
	// Even-length padding on the value read from a file must not defeat the match.
	if !matchesSOPClass("1.2.840.10008.5.1.4.1.1.7\x00", []string{"1.2.840.10008.5.1.4.1.1.7\x00"}) {
		// NUL is not whitespace for TrimSpace; both sides carry it here so the
		// comparison is still exact. The engine's elemStringComponents strips
		// the padding before the value reaches matchesSOPClass.
		t.Error("padded-both-sides match failed")
	}
	if matchesSOPClass("1.2.840.10008.5.1.4.1.1.7.4", list) {
		t.Error("a multi-frame variant must not match the single-frame class by prefix")
	}
	if matchesSOPClass("", list) || matchesSOPClass("1.2.840.10008.5.1.4.1.1.7", nil) {
		t.Error("empty value or empty list must not match")
	}
}
