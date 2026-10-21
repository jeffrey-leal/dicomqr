package main

// Conversion of a profile store written for the retired base-inheritance
// scheme (modifyprofile.go, flattenModProfiles / convertModProfileStore). The
// property that matters is that no run changes: every converted profile must
// compile to exactly the parameters its base chain resolved to.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// legacyStore exercises every merge rule the inheritance scheme had: a
// three-level chain, OR'd booleans, scalars overridden only when set, Sets
// replaced per tag, Removes unioned with a Keep cancelling one (spelled
// differently from the removal), mask regions replaced, filters unioned, and a
// modality both levels override.
func legacyStore() ModProfileConfig {
	return ModProfileConfig{
		"grand": {
			Sets:             []string{"0010,0010=ANON", "0008,0050="},
			Removes:          []string{"0008,0080", "0400,0561", "0010,1000"},
			DOB:              "YYYY0101",
			RemapUIDs:        true,
			Priv:             true,
			NoOverlays:       true,
			AuditTags:        true,
			FixVR:            "correct",
			ExportName:       "[0010,0010]",
			IgnoreModalities: []string{"SR", "PR"},
			MaskRegions:      []MaskRegion{{W: 1, H: 0.08}},
			PerModality: map[string]ModProfile{
				"CT": {Removes: []string{"0018,1030"}},
			},
		},
		"parent": {
			Base:             "grand",
			Sets:             []string{"0010,0010=PARENT"},
			Removes:          []string{"0008,1030"},
			Keep:             []string{"10,1000"},
			ShiftDays:        "-30",
			IgnoreModalities: []string{"pr", "KO"},
			IgnoreSOPClasses: []string{"1.2.840.10008.5.1.4.1.1.7"},
			PerModality: map[string]ModProfile{
				"ct": {Keep: []string{"0008,0080"}},
				"US": {MaskRegions: []MaskRegion{{Mode: maskModeOutsideUS}}},
			},
		},
		"child": {
			Base:           "parent",
			TransferSyntax: tsPrefExplicitLE,
			Zip:            true,
			MaskRegions:    []MaskRegion{{Mode: maskModeOutsideUS}},
		},
		"plain": {Sets: []string{"0010,0010=X"}, Keep: []string{"0010,1000"}, Removes: []string{"0010,1000"}},
	}
}

func TestFlattenPreservesWhatRunsDid(t *testing.T) {
	cfg := legacyStore()
	flat, conv := flattenModProfiles(cfg)

	if want := []string{"child", "parent"}; !slices.Equal(conv.flattened, want) {
		t.Errorf("flattened = %v, want %v", conv.flattened, want)
	}
	if want := []string{"plain"}; !slices.Equal(conv.keepDropped, want) {
		t.Errorf("keepDropped = %v, want %v", conv.keepDropped, want)
	}
	if len(conv.unresolved) > 0 {
		t.Errorf("unresolved = %v", conv.unresolved)
	}

	for name := range cfg {
		p := flat[name]
		if p.Base != "" || len(p.Keep) > 0 {
			t.Errorf("%s: converted profile still carries base %q / keep %v", name, p.Base, p.Keep)
		}
		// What the old scheme ran: the chain resolved, as runs resolved it.
		before, err := resolveBaseChain(name, cfg)
		if err != nil {
			t.Fatalf("%s: resolve: %v", name, err)
		}
		want, err := compileModifyParams(before)
		if err != nil {
			t.Fatalf("%s: compile resolved: %v", name, err)
		}
		got, err := compileModifyParams(p)
		if err != nil {
			t.Fatalf("%s: compile converted: %v", name, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: converted profile compiles differently from what it ran as:\n got  %+v\n want %+v", name, got, want)
		}
	}

	// Spot checks, so the test does not rest on resolveBaseChain alone.
	child := flat["child"]
	if !child.RemapUIDs || !child.Priv || !child.NoOverlays || !child.AuditTags || !child.Zip {
		t.Errorf("child lost an inherited switch: %+v", child)
	}
	if child.DOB != "YYYY0101" || child.ShiftDays != "-30" || child.FixVR != "correct" ||
		child.TransferSyntax != tsPrefExplicitLE || child.ExportName != "[0010,0010]" {
		t.Errorf("child scalars = dob %q shift %q fixvr %q ts %q export %q",
			child.DOB, child.ShiftDays, child.FixVR, child.TransferSyntax, child.ExportName)
	}
	if slices.Contains(child.Removes, "0010,1000") {
		t.Error("the parent's Keep no longer cancels the grandparent's removal")
	}
	if !slices.Contains(child.Removes, "0008,1030") || !slices.Contains(child.Removes, "0400,0561") {
		t.Errorf("child removes = %v, want both levels' removals", child.Removes)
	}
	if want := []string{"0010,0010=PARENT", "0008,0050="}; !slices.Equal(sortedCopy(child.Sets), sortedCopy(want)) {
		t.Errorf("child sets = %v, want the parent's Patient Name replacing the grandparent's", child.Sets)
	}
	if len(child.MaskRegions) != 1 || maskRegionMode(child.MaskRegions[0]) != maskModeOutsideUS {
		t.Errorf("child mask regions = %+v, want its own rule replacing the grandparent's", child.MaskRegions)
	}
	if ct := child.PerModality["CT"]; !slices.Contains(ct.Removes, "0018,1030") || !slices.Contains(ct.Keep, "0008,0080") {
		t.Errorf("child CT override = %+v, want both levels merged", ct)
	}

	// The input is not modified.
	if !reflect.DeepEqual(cfg, legacyStore()) {
		t.Error("flattenModProfiles modified its input")
	}
}

func sortedCopy(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

// A converted profile shares nothing with its base: editing one later must not
// reach into the other through a shared slice.
func TestFlattenedProfilesAreIndependent(t *testing.T) {
	flat, _ := flattenModProfiles(legacyStore())
	child, grand := flat["child"], flat["grand"]
	child.Removes[0] = "FFFF,FFFF"
	child.PerModality["CT"].Removes[0] = "FFFF,FFFF"
	if grand.Removes[0] == "FFFF,FFFF" || grand.PerModality["CT"].Removes[0] == "FFFF,FFFF" {
		t.Error("a converted profile shares storage with its former base")
	}
}

// A chain that does not resolve is left exactly as written and refused at run
// time: guessing what it inherited would run a weaker profile than the one
// written.
func TestFlattenLeavesUnresolvableProfiles(t *testing.T) {
	cfg := ModProfileConfig{
		"orphan": {Base: "gone", Sets: []string{"0010,0010=X"}},
		"loopA":  {Base: "loopB", Removes: []string{"0008,0080"}},
		"loopB":  {Base: "loopA", Removes: []string{"0008,0081"}},
		"ok":     {Sets: []string{"0010,0010=Y"}},
	}
	flat, conv := flattenModProfiles(cfg)
	if conv.changed() {
		t.Errorf("conversion reports a change: %+v", conv)
	}
	for _, name := range []string{"orphan", "loopA", "loopB"} {
		if conv.unresolved[name] == nil {
			t.Errorf("%s not reported unresolved", name)
		}
		if !reflect.DeepEqual(flat[name], cfg[name]) {
			t.Errorf("%s was altered: %+v", name, flat[name])
		}
		_, err := lookupModProfile(name, flat)
		if err == nil || !strings.Contains(err.Error(), "could not be resolved") {
			t.Errorf("%s: lookup error = %v, want a refusal", name, err)
		}
	}
	if _, err := lookupModProfile("ok", flat); err != nil {
		t.Errorf("ok: %v", err)
	}
}

// The startup conversion writes the store once, backing up the original byte
// for byte first, and is a no-op from then on.
func TestConvertModProfileStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	original := []byte(`{"base-deident":{"set":["10,10=ANON"],"remove":["8,80"],"remapuids":true,"noprivate":true},` +
		`"deident-US":{"base":"base-deident","maskregions":[{"mode":"outside-us-regions"}]}}`)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}

	conv, backup, err := convertModProfileStore(path)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !slices.Equal(conv.flattened, []string{"deident-US"}) {
		t.Errorf("flattened = %v", conv.flattened)
	}
	if backup == "" || filepath.Dir(backup) != dir {
		t.Fatalf("backup = %q, want a file beside the store", backup)
	}
	if got, err := os.ReadFile(backup); err != nil || !bytes.Equal(got, original) {
		t.Errorf("backup does not hold the original bytes (err %v)", err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(written, []byte(`"base"`)) {
		t.Errorf("converted store still names a base:\n%s", written)
	}
	cfg, err := loadModProfileConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	us := cfg["deident-US"]
	if !us.RemapUIDs || !us.Priv || !slices.Equal(us.Removes, []string{"0008,0080"}) || len(us.MaskRegions) != 1 {
		t.Errorf("converted deident-US = %+v, want base-deident's settings plus its mask rule", us)
	}

	// Already converted: nothing written, no second backup.
	conv2, backup2, err := convertModProfileStore(path)
	if err != nil || backup2 != "" || conv2.changed() {
		t.Errorf("second conversion: backup %q changed %v err %v, want a no-op", backup2, conv2.changed(), err)
	}
	if again, _ := os.ReadFile(path); !bytes.Equal(again, written) {
		t.Error("second conversion rewrote the store")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("directory holds %d files, want the store and one backup", len(entries))
	}
}

// A store that needs no conversion is never touched — not even reformatted —
// and neither is one whose only base references cannot be resolved.
func TestConvertModProfileStoreLeavesOtherStoresAlone(t *testing.T) {
	for name, content := range map[string]string{
		"self-contained": `{ "p" : { "set" : [ "0010,0010=X" ] } }`,
		"unresolvable":   `{ "p" : { "base" : "gone", "set" : [ "0010,0010=X" ] } }`,
		"unparsable":     `{ "p" : `,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "profiles.json")
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			_, backup, err := convertModProfileStore(path)
			if name == "unparsable" {
				if err == nil {
					t.Error("an unparsable store converted without error")
				}
			} else if err != nil {
				t.Fatalf("convert: %v", err)
			}
			if backup != "" {
				t.Errorf("backup %q written", backup)
			}
			if got, _ := os.ReadFile(path); string(got) != content {
				t.Errorf("store rewritten:\n%s", got)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 1 {
				t.Errorf("directory holds %d files, want only the store", len(entries))
			}
		})
	}
}

// Reading converts in memory too, so a store the startup conversion could not
// write still never reaches a run unconverted.
func TestLoadModProfileConfigConvertsInMemory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.json")
	content := `{"b":{"remove":["0008,0080"],"remapuids":true},"d":{"base":"b","dob":"YYYY0101"}}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadModProfileConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	d, err := lookupModProfile("d", cfg)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !d.RemapUIDs || !slices.Equal(d.Removes, []string{"0008,0080"}) || d.DOB != "YYYY0101" {
		t.Errorf("d = %+v, want b's settings plus its own", d)
	}
	if got, _ := os.ReadFile(path); string(got) != content {
		t.Error("loading rewrote the store")
	}
}

// Field hook: DICOMQR_PROFILES_SAMPLE=<profiles.json> go test -run
// TestSampleProfileStoreConversion -v . converts a copy of a real store (the
// original is only read) and asserts every profile compiles exactly as its
// base chain resolved — what the first start of this version will do to it.
func TestSampleProfileStoreConversion(t *testing.T) {
	src := os.Getenv("DICOMQR_PROFILES_SAMPLE")
	if src == "" {
		t.Skip("DICOMQR_PROFILES_SAMPLE not set")
	}
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "profiles.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	var raw ModProfileConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for name, p := range raw {
		raw[name] = normalizeModProfile(p)
	}

	conv, backup, err := convertModProfileStore(path)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	t.Logf("flattened %v, keep dropped %v, unresolved %v, backup written %v",
		conv.flattened, conv.keepDropped, conv.unresolved, backup != "")
	converted, err := loadModProfileConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	for name := range raw {
		if conv.unresolved[name] != nil {
			continue
		}
		before, err := resolveBaseChain(name, raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want, werr := compileModifyParams(before)
		got, gerr := compileModifyParams(converted[name])
		if (werr == nil) != (gerr == nil) || !reflect.DeepEqual(got, want) {
			t.Errorf("%s compiles differently after conversion (errors %v / %v)", name, werr, gerr)
		}
		t.Logf("%s: %d set, %d remove, base %q -> self-contained", name,
			len(converted[name].Sets), len(converted[name].Removes), raw[name].Base)
	}
}

func TestCopyProfileName(t *testing.T) {
	cfg := ModProfileConfig{"a": {}, "a copy": {}, "a copy 2": {}, "b": {}}
	for src, want := range map[string]string{"a": "a copy 3", "b": "b copy", "a copy": "a copy copy"} {
		if got := copyProfileName(src, cfg); got != want {
			t.Errorf("copyProfileName(%q) = %q, want %q", src, got, want)
		}
	}
}

func TestCloneModProfileIsDeep(t *testing.T) {
	src := ModProfile{
		Sets:        []string{"0010,0010=X"},
		Removes:     []string{"0008,0080"},
		MaskRegions: []MaskRegion{{W: 0.5, H: 0.1, AppliesTo: &MaskScope{Modality: "US", Cols: 800}}},
		PerModality: map[string]ModProfile{"CT": {Removes: []string{"0018,1030"}}},
		RemapUIDs:   true,
		Verbose:     true,
	}
	c := cloneModProfile(src)
	if !reflect.DeepEqual(c, src) {
		t.Fatalf("clone differs:\n got  %+v\n want %+v", c, src)
	}
	c.Sets[0] = "changed"
	c.Removes[0] = "changed"
	c.MaskRegions[0].AppliesTo.Modality = "CT"
	c.PerModality["CT"].Removes[0] = "changed"
	c.PerModality["MR"] = ModProfile{}
	if src.Sets[0] != "0010,0010=X" || src.Removes[0] != "0008,0080" ||
		src.MaskRegions[0].AppliesTo.Modality != "US" || src.PerModality["CT"].Removes[0] != "0018,1030" ||
		len(src.PerModality) != 1 {
		t.Errorf("changing the clone reached the source: %+v", src)
	}
}
