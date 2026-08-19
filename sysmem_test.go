package main

import "testing"

// TestClampInt64 covers the arithmetic the budget sizing rests on, independent
// of what this machine happens to have installed.
func TestClampInt64(t *testing.T) {
	for _, tc := range []struct{ v, lo, hi, want int64 }{
		{5, 10, 100, 10},    // below the floor
		{500, 10, 100, 100}, // above the ceiling
		{50, 10, 100, 50},   // inside
		{10, 10, 100, 10},   // on the floor
		{100, 10, 100, 100}, // on the ceiling
	} {
		if got := clampInt64(tc.v, tc.lo, tc.hi); got != tc.want {
			t.Errorf("clampInt64(%d, %d, %d) = %d, want %d", tc.v, tc.lo, tc.hi, got, tc.want)
		}
	}
}

// TestModifyMemoryBudgetSizing drives the sizing rule over a range of machines
// without asking the real one, so the clamps are tested rather than this
// workstation's RAM.
func TestModifyMemoryBudgetSizing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		total int64
		want  int64
	}{
		// A quarter only clears the 512 MB floor from 2 GB up, so only a very
		// small or virtualised machine is clamped upward.
		{"1 GB VM clamps up to the floor", 1 << 30, modifyMemoryFloor},
		{"2 GB machine lands exactly on the floor", 2 << 30, modifyMemoryFloor},
		{"4 GB laptop takes its quarter", 4 << 30, 1 << 30},
		{"8 GB machine takes its quarter", 8 << 30, 2 << 30},
		{"16 GB machine lands exactly on the ceiling", 16 << 30, modifyMemoryCeiling},
		{"128 GB tower clamps to the ceiling", 128 << 30, modifyMemoryCeiling},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := clampInt64(tc.total/modifyMemoryShare, modifyMemoryFloor, modifyMemoryCeiling)
			if got != tc.want {
				t.Errorf("budget for %d GB = %d MB, want %d MB",
					tc.total>>30, got>>20, tc.want>>20)
			}
		})
	}
}

// TestModifyMemoryBudgetOverride: an explicit override is honoured as typed,
// deliberately without clamping — someone setting it is diagnosing something.
func TestModifyMemoryBudgetOverride(t *testing.T) {
	t.Setenv("DICOMQR_MODIFY_BUDGET_MB", "7")
	if got := resolveModifyMemoryBudget(); got != 7<<20 {
		t.Errorf("override budget = %d bytes, want %d", got, 7<<20)
	}

	// Nonsense falls through to the machine-derived value rather than taking a
	// zero budget, which would stall every run.
	t.Setenv("DICOMQR_MODIFY_BUDGET_MB", "not-a-number")
	if got := resolveModifyMemoryBudget(); got < modifyMemoryFloor {
		t.Errorf("budget with a malformed override = %d, want at least the %d floor",
			got, modifyMemoryFloor)
	}
}

// TestTotalPhysicalMemory is the one test that does touch the real machine: the
// syscall has to actually work here, or the budget silently falls back to its
// floor on every install.
func TestTotalPhysicalMemory(t *testing.T) {
	total := totalPhysicalMemory()
	if total <= 0 {
		t.Fatal("GlobalMemoryStatusEx returned nothing — the budget would fall back on every machine")
	}
	if total < 512<<20 {
		t.Errorf("installed memory reported as %d MB, which is not plausible", total>>20)
	}
	t.Logf("installed memory %d MB, budget %d MB", total>>20, modifyMemoryBudget>>20)
}
