package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"fyne.io/fyne/v2/test"
)

func TestCascadedPlacement(t *testing.T) {
	area := winRect{Left: 0, Top: 0, Right: 1920, Bottom: 1040}
	saved := windowPlacement{X: 100, Y: 100, W: 600, H: 400}

	if x, y, w, h := cascadedPlacement(saved, nil, area); x != 100 || y != 100 || w != 600 || h != 400 {
		t.Errorf("unobstructed: %d,%d %dx%d", x, y, w, h)
	}
	// A sibling where it would open: stepped down and right, twice for two.
	one := []winRect{{Left: 100, Top: 100, Right: 700, Bottom: 500}}
	if x, y, _, _ := cascadedPlacement(saved, one, area); x != 132 || y != 132 {
		t.Errorf("one sibling: opened at %d,%d, want 132,132", x, y)
	}
	two := append(one, winRect{Left: 132, Top: 132, Right: 732, Bottom: 532})
	if x, y, _, _ := cascadedPlacement(saved, two, area); x != 164 || y != 164 {
		t.Errorf("two siblings: opened at %d,%d, want 164,164", x, y)
	}
	// A sibling well away from the spot does not move it.
	far := []winRect{{Left: 900, Top: 300, Right: 1500, Bottom: 700}}
	if x, y, _, _ := cascadedPlacement(saved, far, area); x != 100 || y != 100 {
		t.Errorf("distant sibling moved it to %d,%d", x, y)
	}
	// Off the work area (a monitor since unplugged, nearest area given) and too
	// big for it: brought wholly inside.
	big := windowPlacement{X: 3000, Y: -500, W: 2500, H: 900}
	x, y, w, h := cascadedPlacement(big, nil, area)
	if w != 1920 || h != 900 || x != 0 || y != 0 {
		t.Errorf("off screen and oversized: %d,%d %dx%d", x, y, w, h)
	}
	// Pinned in the bottom-right corner, every step clamps back onto the
	// sibling: it gives up rather than looping.
	corner := windowPlacement{X: 1320, Y: 640, W: 600, H: 400}
	pinned := []winRect{{Left: 1320, Top: 640, Right: 1920, Bottom: 1040}}
	if x, y, _, _ := cascadedPlacement(corner, pinned, area); x != 1320 || y != 640 {
		t.Errorf("pinned corner: %d,%d", x, y)
	}
}

// The store hands back copies, starts from what settings held, and stays out
// of settings.json when empty.
func TestWindowPlacementStore(t *testing.T) {
	saved := map[string]windowPlacement{"viewer": {X: 10, Y: 20, W: 640, H: 720}}
	initWindowPlacements(saved)
	defer initWindowPlacements(nil)
	got := snapshotWindowPlacements()
	if !reflect.DeepEqual(got, saved) {
		t.Errorf("snapshot %v, want %v", got, saved)
	}
	got["viewer"] = windowPlacement{}
	saved["viewer"] = windowPlacement{}
	if again := snapshotWindowPlacements(); again["viewer"].W != 640 {
		t.Errorf("the store shares its map with a caller: %v", again)
	}
	initWindowPlacements(nil)
	if snap := snapshotWindowPlacements(); snap != nil {
		t.Errorf("empty store snapshot = %v, want nil (omitted from settings)", snap)
	}
}

// Under a driver with no native window there is nothing to place or record,
// and nothing is registered.
func TestPlaceWindowWithoutNativeWindow(t *testing.T) {
	a := test.NewApp()
	initWindowPlacements(map[string]windowPlacement{"viewer": {X: 1, Y: 1, W: 100, H: 100}})
	defer initWindowPlacements(nil)
	win := a.NewWindow("x")
	showPlacedWindow(win, "viewer")
	placementMu.Lock()
	_, registered := placedWindows[win]
	placementMu.Unlock()
	if registered {
		t.Errorf("a window with no native handle was registered")
	}
	win.Close()
}

func TestWindowPlacementsSettingsJSON(t *testing.T) {
	s := Settings{WindowPlacements: map[string]windowPlacement{"preferences": {X: -1200, Y: 40, W: 720, H: 640}}}
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"windowPlacements":{"preferences":{"x":-1200,"y":40,"width":720,"height":640}}`) {
		t.Errorf("marshalled %s", data)
	}
	var back Settings
	if err := json.Unmarshal(data, &back); err != nil || !reflect.DeepEqual(back.WindowPlacements, s.WindowPlacements) {
		t.Errorf("round trip %v (%v)", back.WindowPlacements, err)
	}
	none, _ := json.Marshal(Settings{})
	if strings.Contains(string(none), "windowPlacements") {
		t.Errorf("an empty store writes the key")
	}
}

// Every window opened through openOwnedWindow names a Place, so a window added
// later cannot silently opt out of being remembered — and a Place is a plain
// string literal, never built from a title that could carry patient data.
func TestEveryOwnedWindowHasAPlace(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	found := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "windowSpec" {
				return true
			}
			found++
			for _, e := range lit.Elts {
				kv, ok := e.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Place" {
					if bl, ok := kv.Value.(*ast.BasicLit); !ok || bl.Kind != token.STRING {
						t.Errorf("%s: Place is not a string literal", fset.Position(kv.Pos()))
					}
					return true
				}
			}
			t.Errorf("%s: windowSpec without a Place", fset.Position(lit.Pos()))
			return true
		})
	}
	if found < 12 {
		t.Errorf("found only %d windowSpec literals; the scan is not seeing the callers", found)
	}
}
