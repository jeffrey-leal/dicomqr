package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The user's order first, then everything it does not name alphabetically; a
// stale or repeated name never produces a phantom or duplicate entry.
func TestOrderedModProfileNames(t *testing.T) {
	cfg := ModProfileConfig{"base-deident": {}, "US-deident": {}, "de-ident-baseline": {}, "zz-test": {}}
	for _, tc := range []struct {
		name  string
		order []string
		want  []string
	}{
		{"no order is alphabetical", nil,
			[]string{"US-deident", "base-deident", "de-ident-baseline", "zz-test"}},
		{"ordered first, the rest after", []string{"de-ident-baseline", "zz-test"},
			[]string{"de-ident-baseline", "zz-test", "US-deident", "base-deident"}},
		{"deleted and repeated names skipped", []string{"gone", "zz-test", "base-deident", "zz-test"},
			[]string{"zz-test", "base-deident", "US-deident", "de-ident-baseline"}},
		{"a full order is kept as given", []string{"zz-test", "de-ident-baseline", "base-deident", "US-deident"},
			[]string{"zz-test", "de-ident-baseline", "base-deident", "US-deident"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := orderedModProfileNames(cfg, tc.order); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
	if got := orderedModProfileNames(ModProfileConfig{}, []string{"a"}); len(got) != 0 {
		t.Errorf("empty store gave %v", got)
	}
}

// An alphabetical list stores nothing, so a user who never reorders keeps
// alphabetical order for profiles added later; any other order is stored as a
// copy the caller can go on editing.
func TestStoredModProfileOrder(t *testing.T) {
	if got := storedModProfileOrder([]string{"US-deident", "base-deident", "de-ident-baseline"}); got != nil {
		t.Errorf("alphabetical order stored as %v", got)
	}
	in := []string{"de-ident-baseline", "US-deident"}
	got := storedModProfileOrder(in)
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("stored %v", got)
	}
	in[0] = "changed"
	if got[0] != "de-ident-baseline" {
		t.Errorf("stored order shares the caller's slice")
	}

	// And the setting stays out of settings.json until there is an order.
	data, _ := json.Marshal(Settings{})
	if strings.Contains(string(data), "modProfileOrder") {
		t.Errorf("an unset order is written: %s", data)
	}
	data, _ = json.Marshal(Settings{ModProfileOrder: got})
	var back Settings
	if err := json.Unmarshal(data, &back); err != nil || !reflect.DeepEqual(back.ModProfileOrder, got) {
		t.Errorf("order did not round-trip: %v (%v)", back.ModProfileOrder, err)
	}
}
