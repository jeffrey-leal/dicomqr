package main

// MR phases: a series of single-frame instances that covers the same stack of
// slice positions several times over — in-phase/out-of-phase Dixon pairs,
// diffusion b-values, dynamic contrast timepoints — presented as K phases of N
// slices instead of one flat K×N slider.
//
// Without this, an in/out-phase abdomen series (200 files = 2 echoes × 100
// slices) scrolls through all of phase 1 and then starts over at the top of
// phase 2; comparing the two phases at one anatomical position — the entire
// point of such a sequence — means scrubbing exactly 100 positions away and
// back. In phase mode the slider spans one phase's slices and a dropdown (or
// the P key) flips between phases at the same slice.
//
// Detection is structural, not tag-driven: the phase axis is whatever makes
// the instance-ordered slice locations repeat as K identical runs. Real series
// force this choice — the sample study's in/out pair is distinguished by
// EchoNumbers, but its diffusion series carries identical echo attributes on
// both b-values (only SequenceName differs, and the standard DiffusionBValue
// tag is empty), so no single tag identifies the axis reliably. Tags are used
// only to *name* the phases once found.

import (
	"fmt"
	"math"
	"strings"
)

// mrPhase is one phase of a phased series: a label for the dropdown and the
// phase's slices in stack order. Every phase of a series has the same length,
// and index i addresses the same anatomical position in each phase — that is
// what detection guarantees, and what makes phase switching a same-slice
// toggle.
type mrPhase struct {
	label  string
	slices []viewerSlice
}

// sliceLocTolerance is the maximum difference (mm) for two SliceLocation
// values to count as the same position. Real repeats agree to the micrometre;
// the tolerance only absorbs decimal-string round-tripping.
const sliceLocTolerance = 0.01

// detectMRPhases reports the phase structure of a series, or nil when it has
// none and the flat slice list should be kept. chapters must be in the
// viewer's presentation order (InstanceNumber).
//
// Requirements, all conservative — anything irregular falls back to the flat
// list rather than guessing: every instance single-frame with a SliceLocation
// and one shared ImageOrientationPatient (a localizer's mixed planes must not
// be mistaken for phases); the location sequence must partition exactly into
// K ≥ 2 identical runs of N ≥ 2 distinct positions, either blocked
// (s1..sN, s1..sN, ...) or interleaved (each position's K phases adjacent).
func detectMRPhases(chapters []chapter) []mrPhase {
	if len(chapters) < 4 {
		return nil
	}
	orientation := chapters[0].orientation
	if orientation == "" {
		return nil
	}
	locs := make([]float64, len(chapters))
	for i, c := range chapters {
		if c.frames != 1 || !c.hasSliceLoc || c.orientation != orientation {
			return nil
		}
		locs[i] = c.sliceLoc
	}

	if groups := phaseRunsBlocked(locs); groups != nil {
		return buildPhases(chapters, groups)
	}
	if groups := phaseRunsInterleaved(locs); groups != nil {
		return buildPhases(chapters, groups)
	}
	return nil
}

func sameLoc(a, b float64) bool { return math.Abs(a-b) <= sliceLocTolerance }

// phaseRunsBlocked matches s1..sN repeated K times and returns each phase's
// indices, or nil.
func phaseRunsBlocked(locs []float64) [][]int {
	// The run length is where the first position recurs.
	n := 0
	for i := 1; i < len(locs); i++ {
		if sameLoc(locs[i], locs[0]) {
			n = i
			break
		}
	}
	if n < 2 || len(locs)%n != 0 {
		return nil
	}
	k := len(locs) / n
	if k < 2 {
		return nil
	}
	// Every later run must repeat the first exactly, and the first run's
	// positions must be pairwise distinct (a repeated position inside a run
	// would make slice indices ambiguous).
	for i := n; i < len(locs); i++ {
		if !sameLoc(locs[i], locs[i%n]) {
			return nil
		}
	}
	if !pairwiseDistinct(locs[:n]) {
		return nil
	}

	groups := make([][]int, k)
	for p := 0; p < k; p++ {
		groups[p] = make([]int, n)
		for s := 0; s < n; s++ {
			groups[p][s] = p*n + s
		}
	}
	return groups
}

// phaseRunsInterleaved matches each position's K phases stored adjacently
// (s1p1, s1p2, ..., s2p1, s2p2, ...) and returns each phase's indices, or nil.
func phaseRunsInterleaved(locs []float64) [][]int {
	// The phase count is how many leading files share the first position.
	k := 1
	for k < len(locs) && sameLoc(locs[k], locs[0]) {
		k++
	}
	if k < 2 || len(locs)%k != 0 {
		return nil
	}
	n := len(locs) / k
	if n < 2 {
		return nil
	}
	chunkLocs := make([]float64, n)
	for s := 0; s < n; s++ {
		chunkLocs[s] = locs[s*k]
		for i := s * k; i < (s+1)*k; i++ {
			if !sameLoc(locs[i], chunkLocs[s]) {
				return nil
			}
		}
	}
	if !pairwiseDistinct(chunkLocs) {
		return nil
	}

	groups := make([][]int, k)
	for p := 0; p < k; p++ {
		groups[p] = make([]int, n)
		for s := 0; s < n; s++ {
			groups[p][s] = s*k + p
		}
	}
	return groups
}

func pairwiseDistinct(locs []float64) bool {
	for i := range locs {
		for j := i + 1; j < len(locs); j++ {
			if sameLoc(locs[i], locs[j]) {
				return false
			}
		}
	}
	return true
}

func buildPhases(chapters []chapter, groups [][]int) []mrPhase {
	labels := phaseLabels(chapters, groups)
	phases := make([]mrPhase, len(groups))
	for p, idxs := range groups {
		slices := make([]viewerSlice, len(idxs))
		for s, i := range idxs {
			slices[s] = viewerSlice{path: chapters[i].path, frame: 0}
		}
		phases[p] = mrPhase{label: labels[p], slices: slices}
	}
	return phases
}

// phaseLabels names each phase from the first attribute that distinguishes the
// groups, in order of how meaningfully it reads: echo number/time (in/out
// phase pairs), the vendor pulse-sequence name (diffusion b-values on Siemens:
// *ep_b50t / *ep_b800t), temporal position, acquisition number — and a plain
// ordinal when nothing does. Labels are forced unique because the dropdown
// selects by string.
func phaseLabels(chapters []chapter, groups [][]int) []string {
	reps := make([]chapter, len(groups))
	for p, idxs := range groups {
		reps[p] = chapters[idxs[0]]
	}
	differs := func(get func(chapter) string) bool {
		for _, r := range reps[1:] {
			if get(r) != get(reps[0]) {
				return true
			}
		}
		return false
	}

	labels := make([]string, len(reps))
	switch {
	case differs(func(c chapter) string { return fmt.Sprintf("%d|%s", c.echoNumber, c.echoTime) }):
		for p, r := range reps {
			switch {
			case r.echoNumber > 0 && r.echoTime != "":
				labels[p] = fmt.Sprintf("Echo %d (TE %s ms)", r.echoNumber, r.echoTime)
			case r.echoTime != "":
				labels[p] = fmt.Sprintf("TE %s ms", r.echoTime)
			default:
				labels[p] = fmt.Sprintf("Echo %d", r.echoNumber)
			}
		}
	case differs(func(c chapter) string { return c.seqName }):
		for p, r := range reps {
			labels[p] = strings.TrimPrefix(r.seqName, "*")
		}
	case differs(func(c chapter) string { return fmt.Sprint(c.temporalPos) }):
		for p, r := range reps {
			labels[p] = fmt.Sprintf("Phase %d", r.temporalPos)
		}
	case differs(func(c chapter) string { return fmt.Sprint(c.acqNumber) }):
		for p, r := range reps {
			labels[p] = fmt.Sprintf("Acquisition %d", r.acqNumber)
		}
	default:
		for p := range reps {
			labels[p] = fmt.Sprintf("Phase %d", p+1)
		}
	}

	// The Select widget identifies options by string, so duplicates (or empty
	// fallbacks) must be disambiguated.
	seen := map[string]bool{}
	for p, l := range labels {
		if l == "" || seen[l] {
			labels[p] = fmt.Sprintf("%d: %s", p+1, l)
		}
		seen[labels[p]] = true
	}
	return labels
}
