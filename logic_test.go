package main

import (
	"strings"
	"testing"
)

// ── sanitize ─────────────────────────────────────────────────────────────────

func TestSanitize(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"normal", "normal"},
		{"with space", "with space"},
		{`back\slash`, "back_slash"},
		{"for/ward", "for_ward"},
		{"col:on", "col_on"},
		{"ast*risk", "ast_risk"},
		{"ques?tion", "ques_tion"},
		{`quo"te`, "quo_te"},
		{"less<than", "less_than"},
		{"grea>ter", "grea_ter"},
		{"pi|pe", "pi_pe"},
		{"CON", "_CON"},   // Windows reserved name
		{"con", "_con"},   // case-insensitive
		{"NUL", "_NUL"},
		{"COM1", "_COM1"},
		{"LPT9", "_LPT9"},
		{"CONMAN", "CONMAN"}, // reserved prefix but not exact match — allowed
		{"", ""},
	}
	for _, tc := range tests {
		got := sanitize(tc.in)
		if got != tc.want {
			t.Errorf("sanitize(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

// ── truncateRunes ─────────────────────────────────────────────────────────────

func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		in      string
		max     int
		want    string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello", 4, "hell"},
		{"hello", 0, ""},
		// Multi-byte Unicode: each kanji is 3 bytes but 1 rune.
		{"日本語テスト", 4, "日本語テ"},
		{"日本語テスト", 6, "日本語テスト"},
		{"日本語テスト", 7, "日本語テスト"},
		{"", 5, ""},
	}
	for _, tc := range tests {
		got := truncateRunes(tc.in, tc.max)
		if got != tc.want {
			t.Errorf("truncateRunes(%q, %d) = %q; want %q", tc.in, tc.max, got, tc.want)
		}
	}
}

// ── organizeFilePath ──────────────────────────────────────────────────────────

func TestOrganizeFilePath_Normal(t *testing.T) {
	got := organizeFilePath(`C:\Downloads`, "Doe^John", "MRN123", "Chest CT", "20240115", "Series 1", "2", "1.2.3.4.5")
	// Should contain patient, study, series folders and .dcm extension.
	for _, substr := range []string{"Doe^John", "MRN123", "Chest CT", "20240115", "Series 1", "1.2.3.4.5.dcm"} {
		if !contains(got, substr) {
			t.Errorf("organizeFilePath: expected %q in path %q", substr, got)
		}
	}
}

func TestOrganizeFilePath_MissingFields(t *testing.T) {
	got := organizeFilePath(`C:\Downloads`, "", "", "", "", "", "", "uid123")
	for _, substr := range []string{"Unknown Patient", "Unknown Study", "Unknown Series"} {
		if !contains(got, substr) {
			t.Errorf("organizeFilePath (missing fields): expected %q in path %q", substr, got)
		}
	}
}

func TestOrganizeFilePath_LongPathFallback(t *testing.T) {
	// Use a base path + long name combo that exceeds 255 chars even after truncation.
	// Each folder component is truncated to 64 runes; with a 50-char base,
	// 3×64-char folders + separators + a long UID filename pushes well past 255.
	base := `C:\Users\SomeLongUsername\Documents\DICOM\Downloads`
	longName := "A very long patient name that keeps going on and on to force truncation XYZ"
	longUID := "1.2.840.10008.5.1.4.1.1.2.123456789012.123456789012.123456789012.9999"
	got := organizeFilePath(base, longName, longName, longName, "20240101", longName, "1", longUID)
	// Flat fallback: result must be ≤ 255 chars and sit directly under base.
	if len(got) > 255 {
		t.Errorf("organizeFilePath long path: result %d chars, expected ≤ 255\npath: %s", len(got), got)
	}
	// The flat fallback places the file directly under downloadDir.
	if !strings.HasPrefix(got, base) {
		t.Errorf("organizeFilePath long path: expected prefix %q, got %q", base, got)
	}
}

// ── buildDateRange ─────────────────────────────────────────────────────────────

func TestBuildDateRange(t *testing.T) {
	tests := []struct {
		from, to, want string
	}{
		{"", "", ""},
		{"20240101", "", "20240101-"},
		{"", "20240131", "-20240131"},
		{"20240101", "20240131", "20240101-20240131"},
	}
	for _, tc := range tests {
		got := buildDateRange(tc.from, tc.to)
		if got != tc.want {
			t.Errorf("buildDateRange(%q, %q) = %q; want %q", tc.from, tc.to, got, tc.want)
		}
	}
}

// ── resultsModel ─────────────────────────────────────────────────────────────

func TestResultsModel_PatientSort(t *testing.T) {
	m := newResultsModel()
	m.addStudy("Zebra^Alice", "P3", "S3", "20240101", "", "", "")
	m.addStudy("Apple^Bob", "P1", "S1", "20240101", "", "", "")
	m.addStudy("Mango^Carol", "P2", "S2", "20240101", "", "", "")

	roots := m.childUIDs("")
	if len(roots) != 3 {
		t.Fatalf("expected 3 roots, got %d", len(roots))
	}
	wantOrder := []string{"P:P1", "P:P2", "P:P3"}
	for i, id := range roots {
		if id != wantOrder[i] {
			t.Errorf("root[%d] = %q; want %q", i, id, wantOrder[i])
		}
	}
}

func TestResultsModel_StudyDateSort(t *testing.T) {
	m := newResultsModel()
	m.addStudy("Doe^John", "P1", "S3", "20240301", "", "", "")
	m.addStudy("Doe^John", "P1", "S1", "20240101", "", "", "")
	m.addStudy("Doe^John", "P1", "S2", "20240201", "", "", "")

	studies := m.childUIDs("P:P1")
	if len(studies) != 3 {
		t.Fatalf("expected 3 studies, got %d", len(studies))
	}
	wantOrder := []string{"S:S1", "S:S2", "S:S3"}
	for i, id := range studies {
		if id != wantOrder[i] {
			t.Errorf("study[%d] = %q; want %q", i, id, wantOrder[i])
		}
	}
}

func TestResultsModel_SeriesNumericSort(t *testing.T) {
	m := newResultsModel()
	m.addStudy("Doe^John", "P1", "S1", "20240101", "", "", "")
	m.addSeries("S1", "R10", "CT", "10", "", 0)
	m.addSeries("S1", "R2", "CT", "2", "", 0)
	m.addSeries("S1", "R1", "CT", "1", "", 0)

	series := m.childUIDs("S:S1")
	if len(series) != 3 {
		t.Fatalf("expected 3 series, got %d", len(series))
	}
	wantOrder := []string{"R:R1", "R:R2", "R:R10"}
	for i, id := range series {
		if id != wantOrder[i] {
			t.Errorf("series[%d] = %q; want %q", i, id, wantOrder[i])
		}
	}
}

func TestResultsModel_DuplicateSuppression(t *testing.T) {
	m := newResultsModel()
	m.addStudy("Doe^John", "P1", "S1", "20240101", "", "", "")
	m.addStudy("Doe^John", "P1", "S1", "20240101", "", "", "") // duplicate
	if len(m.roots) != 1 {
		t.Errorf("expected 1 root after duplicate patient, got %d", len(m.roots))
	}
	if len(m.childUIDs("P:P1")) != 1 {
		t.Errorf("expected 1 study after duplicate, got %d", len(m.childUIDs("P:P1")))
	}
}

func TestResultsModel_AddSeries_UnknownStudy(t *testing.T) {
	m := newResultsModel()
	m.addSeries("nonexistent", "R1", "CT", "1", "", 0) // should not panic or add
	if len(m.nodes) != 0 {
		t.Errorf("expected no nodes for unknown study, got %d", len(m.nodes))
	}
}

func TestResultsModel_Filter(t *testing.T) {
	m := newResultsModel()
	m.addStudy("Doe^John", "P1", "S1", "20240101", "Chest CT", "", "")
	m.addStudy("Smith^Jane", "P2", "S2", "20240101", "Brain MRI", "", "")

	m.setFilter("chest")
	filtered := m.activeRoots()
	if len(filtered) != 1 || filtered[0] != "P:P1" {
		t.Errorf("filter 'chest': expected [P:P1], got %v", filtered)
	}

	m.setFilter("smith")
	filtered = m.activeRoots()
	if len(filtered) != 1 || filtered[0] != "P:P2" {
		t.Errorf("filter 'smith': expected [P:P2], got %v", filtered)
	}

	m.setFilter("nomatch")
	filtered = m.activeRoots()
	if len(filtered) != 0 {
		t.Errorf("filter 'nomatch': expected [], got %v", filtered)
	}

	m.setFilter("")
	if m.filtered != nil {
		t.Errorf("empty filter: expected filtered == nil (all visible), got %v", m.filtered)
	}
}

func TestResultsModel_FilterMatchViaChild(t *testing.T) {
	m := newResultsModel()
	m.addStudy("Doe^John", "P1", "S1", "20240101", "Chest CT", "", "")

	// Filter on a study label substring — parent patient should also be visible.
	m.setFilter("chest ct")
	filtered := m.activeRoots()
	if len(filtered) != 1 || filtered[0] != "P:P1" {
		t.Errorf("filter via child: expected [P:P1], got %v", filtered)
	}
}

func contains(s, substr string) bool { return strings.Contains(s, substr) }
