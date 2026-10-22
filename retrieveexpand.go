package main

import (
	"cmp"
	"context"
	"slices"
	"sync"
)

// A retrieve's unit is the target: one C-MOVE or C-GET request for a whole
// study or a single series, run on an association of its own. The scheduler
// (retrievesched.go) spreads targets over the profile's Parallel transfers, so
// a retrieve only ever goes as parallel as it has targets — and a selected
// study used to be one target however large, fetched on one association while
// the setting promised several. A study-level request has no inner parallelism
// to offer: the server's store SCU sends the study's instances one after
// another on one association, waiting for each response, so per-object latency
// sets the pace. The only standard way to spread one study is to ask for its
// series separately — field-measured 2026-10-22 against DICOMSRV: a study
// fetched series by series took half the time of the same study fetched whole
// (user decision: split automatically whenever the profile allows more than
// one association).
//
// expandStudyTargets does that split: each STUDY target becomes one SERIES
// target per series the server lists, largest first, so the scheduler's queue
// gives the long series a head start and the short ones fill in behind it
// (longest-processing-time ordering, for free). The list comes from the same
// SERIES-level C-FIND the results tree runs when a study is expanded, so it is
// the server's own view at retrieve time, never the tree's possibly stale one.
// Compatibility is kept by falling back to the whole study — exactly the
// request that always worked — whenever the split is not clearly better: the
// profile allows one association (then nothing changes at all), the server
// lists one series or none, or the list fails or is cut short (a partial list
// would silently lose the series it did not reach). The information model is
// the caller's check: Patient/Study Only has no SERIES level to ask for.

// retrieveTarget is one unit of a retrieve.
type retrieveTarget struct {
	level     string // "STUDY" or "SERIES"
	patientID string
	studyUID  string
	seriesUID string
	// instances is Number of Series Related Instances as the server listed it
	// for a series split out of a study; 0 when unknown or not reported. It
	// orders the queue and weights the progress bar, nothing else.
	instances int
}

// retrieveSeries is one series as a SERIES-level C-FIND lists it.
type retrieveSeries struct {
	uid       string
	instances int // Number of Series Related Instances; 0 when not reported
}

// seriesLister lists a study's series. An error means the list cannot be
// relied on — never a partial list — and the study is then retrieved whole.
type seriesLister func(ctx context.Context, patientID, studyUID string) ([]retrieveSeries, error)

// expandStudyTargets replaces each STUDY target with one SERIES target per
// listed series, largest first, when limit allows more than one association.
// Studies are listed up to limit at a time (the associations the profile says
// the server takes). SERIES targets already in the list, and studies kept
// whole, stay as they were, and the result keeps the input's study order, so
// the outcome and the retry dialog stay deterministic.
func expandStudyTargets(ctx context.Context, targets []retrieveTarget, limit int, list seriesLister) []retrieveTarget {
	if limit <= 1 || countTargets(targets, "STUDY") == 0 {
		return targets
	}

	expanded := make([][]retrieveTarget, len(targets))
	slots := make(chan struct{}, limit)
	var wg sync.WaitGroup
listing:
	for i, t := range targets {
		if t.level != "STUDY" {
			continue
		}
		// Checked before the select: with the run already cancelled both of
		// its cases are ready and Go picks one at random, which let a
		// cancelled run still fire a query (TestExpandStudyTargetsStopsOnCancel).
		if ctx.Err() != nil {
			break
		}
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			break listing // the run is over; whatever is left stays whole
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			expanded[i] = seriesTargetsFor(ctx, t, list)
		}()
	}
	wg.Wait()

	out := make([]retrieveTarget, 0, len(targets))
	for i, t := range targets {
		if len(expanded[i]) > 0 {
			out = append(out, expanded[i]...)
		} else {
			out = append(out, t)
		}
	}
	return out
}

// seriesTargetsFor lists study t's series and returns them as SERIES targets,
// largest first, or nil when the study is to be retrieved whole.
func seriesTargetsFor(ctx context.Context, t retrieveTarget, list seriesLister) []retrieveTarget {
	series, err := list(ctx, t.patientID, t.studyUID)
	switch {
	case ctx.Err() != nil:
		return nil
	case err != nil:
		logWarn("retrieve: could not list the series of study %s (%v) — retrieving it whole", t.studyUID, err)
		return nil
	}
	series = uniqueSeries(series)
	if len(series) < 2 {
		logInfo("retrieve: study %s lists %d series — retrieving it whole", t.studyUID, len(series))
		return nil
	}
	slices.SortStableFunc(series, func(a, b retrieveSeries) int { return cmp.Compare(b.instances, a.instances) })
	out := make([]retrieveTarget, len(series))
	listed := 0
	for i, s := range series {
		out[i] = retrieveTarget{level: "SERIES", patientID: t.patientID, studyUID: t.studyUID, seriesUID: s.uid, instances: s.instances}
		listed += s.instances
	}
	logInfo("retrieve: study %s has %d series (%d instances listed) — retrieving them separately", t.studyUID, len(series), listed)
	return out
}

// uniqueSeries drops entries without a UID and repeats of one, keeping first
// occurrences in order. A series listed twice would otherwise be moved twice.
func uniqueSeries(series []retrieveSeries) []retrieveSeries {
	seen := make(map[string]bool, len(series))
	out := make([]retrieveSeries, 0, len(series))
	for _, s := range series {
		if s.uid == "" || seen[s.uid] {
			continue
		}
		seen[s.uid] = true
		out = append(out, s)
	}
	return out
}

// countTargets is how many targets are at the given level.
func countTargets(targets []retrieveTarget, level string) int {
	n := 0
	for _, t := range targets {
		if t.level == level {
			n++
		}
	}
	return n
}

// retrieveWeights is each target's share of the progress bar: the instance
// counts the server listed, when every target has one, so a two-image series
// does not move the bar as far as a 500-slice one; otherwise an equal share —
// a study kept whole has no count, and mixing counted and uncounted targets
// would weigh the unknown ones arbitrarily.
func retrieveWeights(targets []retrieveTarget) []float64 {
	w := make([]float64, len(targets))
	for i, t := range targets {
		if t.instances <= 0 {
			for j := range w {
				w[j] = 1
			}
			return w
		}
		w[i] = float64(t.instances)
	}
	return w
}

// retrieveProgress is the bar's position: the weighted mean of the targets'
// own fractions.
func retrieveProgress(fracs, weights []float64) float64 {
	var sum, total float64
	for i, f := range fracs {
		sum += f * weights[i]
		total += weights[i]
	}
	if total == 0 {
		return 0
	}
	return sum / total
}

// listSeries is the retrieve's seriesLister: the SERIES-level C-FIND the
// results tree runs when a study is expanded (buildFindFilter asks for Number
// of Series Related Instances, an optional return key a server may leave
// empty). A query that fails or is cut short returns its error and no list —
// a partial list would drop the series it never reached.
func (c *DicomClient) listSeries(ctx context.Context, _ string, studyUID string) ([]retrieveSeries, error) {
	ch, err := c.Find(ctx, "SERIES", map[string]string{"StudyInstanceUID": studyUID})
	if err != nil {
		return nil, err
	}
	var out []retrieveSeries
	var firstErr error
	for r := range ch {
		if r.Err != nil {
			if firstErr == nil {
				firstErr = r.Err
			}
			continue
		}
		out = append(out, retrieveSeries{uid: r.SeriesInstanceUID, instances: r.NumInstances})
	}
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err // a cancelled query closes its stream without an Err
	}
	return out, nil
}
