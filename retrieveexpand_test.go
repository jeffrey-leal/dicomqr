package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func studyTarget(uid string) retrieveTarget {
	return retrieveTarget{level: "STUDY", patientID: "P1", studyUID: uid}
}

// fixedLister answers from a map; a study not in it fails to list.
func fixedLister(series map[string][]retrieveSeries) seriesLister {
	return func(_ context.Context, _, studyUID string) ([]retrieveSeries, error) {
		s, ok := series[studyUID]
		if !ok {
			return nil, errors.New("query refused")
		}
		return s, nil
	}
}

// At one association nothing changes: the targets come back as they were and
// no series is ever listed, so limit 1 is exactly the old retrieve.
func TestExpandStudyTargetsAtLimitOneIsIdentity(t *testing.T) {
	in := []retrieveTarget{studyTarget("1.1"), {level: "SERIES", patientID: "P1", studyUID: "1.2", seriesUID: "1.2.1"}}
	listed := false
	out := expandStudyTargets(context.Background(), in, 1, func(context.Context, string, string) ([]retrieveSeries, error) {
		listed = true
		return []retrieveSeries{{uid: "a", instances: 1}, {uid: "b", instances: 1}}, nil
	})
	if listed {
		t.Fatal("limit 1 listed a study's series")
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("limit 1 changed the targets: %+v", out)
	}
}

// A study becomes one SERIES target per listed series, largest first, carrying
// the study's identity; an entry without a UID and a repeat of one are dropped.
func TestExpandStudyTargetsSplitsLargestFirst(t *testing.T) {
	lister := fixedLister(map[string][]retrieveSeries{
		"1.1": {
			{uid: "1.1.1", instances: 3},
			{uid: "1.1.2", instances: 10},
			{uid: "1.1.3", instances: 1},
			{uid: "1.1.2", instances: 10},
			{uid: "", instances: 5},
		},
	})
	out := expandStudyTargets(context.Background(), []retrieveTarget{studyTarget("1.1")}, 3, lister)
	want := []retrieveTarget{
		{level: "SERIES", patientID: "P1", studyUID: "1.1", seriesUID: "1.1.2", instances: 10},
		{level: "SERIES", patientID: "P1", studyUID: "1.1", seriesUID: "1.1.1", instances: 3},
		{level: "SERIES", patientID: "P1", studyUID: "1.1", seriesUID: "1.1.3", instances: 1},
	}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("split:\n got %+v\nwant %+v", out, want)
	}
}

// The whole study — the request that always worked — whenever the split is
// not clearly better: one series, none, or a list that cannot be trusted.
func TestExpandStudyTargetsKeepsStudyWholeWhenNotClearlyBetter(t *testing.T) {
	cases := map[string]seriesLister{
		"one series": fixedLister(map[string][]retrieveSeries{"1.1": {{uid: "1.1.1", instances: 40}}}),
		"no series":  fixedLister(map[string][]retrieveSeries{"1.1": nil}),
		"list fails": fixedLister(nil),
	}
	for name, lister := range cases {
		in := []retrieveTarget{studyTarget("1.1")}
		if out := expandStudyTargets(context.Background(), in, 4, lister); !reflect.DeepEqual(out, in) {
			t.Errorf("%s: %+v, want the study kept whole", name, out)
		}
	}
}

// Studies keep their order, SERIES targets already present stay as they are,
// and series of equal size keep the server's order.
func TestExpandStudyTargetsKeepsOrderAndSeriesTargets(t *testing.T) {
	lister := fixedLister(map[string][]retrieveSeries{
		"1.1": {{uid: "1.1.1", instances: 2}, {uid: "1.1.2", instances: 5}},
		"1.3": {{uid: "1.3.1", instances: 1}, {uid: "1.3.2", instances: 1}},
	})
	picked := retrieveTarget{level: "SERIES", patientID: "P1", studyUID: "1.2", seriesUID: "1.2.7"}
	in := []retrieveTarget{studyTarget("1.1"), picked, studyTarget("1.3")}
	out := expandStudyTargets(context.Background(), in, 2, lister)
	want := []retrieveTarget{
		{level: "SERIES", patientID: "P1", studyUID: "1.1", seriesUID: "1.1.2", instances: 5},
		{level: "SERIES", patientID: "P1", studyUID: "1.1", seriesUID: "1.1.1", instances: 2},
		picked,
		{level: "SERIES", patientID: "P1", studyUID: "1.3", seriesUID: "1.3.1", instances: 1},
		{level: "SERIES", patientID: "P1", studyUID: "1.3", seriesUID: "1.3.2", instances: 1},
	}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("order:\n got %+v\nwant %+v", out, want)
	}
}

// Studies are listed up to the limit at once — the associations the profile
// says the server takes — and never more. The first three listings hold each
// other until all three have begun (bounded, so a limit that admits fewer
// fails the peak check instead of hanging), which makes the overlap a fact
// rather than a race against the scheduler.
func TestExpandStudyTargetsListsUpToLimitAtOnce(t *testing.T) {
	var m concurrencyMeter
	var begun atomic.Int32
	released := make(chan struct{})
	lister := func(_ context.Context, _, uid string) ([]retrieveSeries, error) {
		m.enter()
		defer m.leave()
		if begun.Add(1) == 3 {
			close(released)
		}
		select {
		case <-released:
		case <-time.After(2 * time.Second):
		}
		return []retrieveSeries{{uid: uid + ".1", instances: 1}, {uid: uid + ".2", instances: 1}}, nil
	}
	var in []retrieveTarget
	for i := range 9 {
		in = append(in, studyTarget(fmt.Sprintf("1.%d", i)))
	}
	out := expandStudyTargets(context.Background(), in, 3, lister)
	if got := int(m.peak.Load()); got != 3 {
		t.Errorf("peak concurrent listings %d, want 3", got)
	}
	if len(out) != 18 {
		t.Errorf("%d targets, want 18", len(out))
	}
}

// A cancelled run lists nothing and keeps every study whole; the scheduler
// then cancels them all, as it would have.
func TestExpandStudyTargetsStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var calls atomic.Int32
	lister := func(context.Context, string, string) ([]retrieveSeries, error) {
		calls.Add(1)
		return []retrieveSeries{{uid: "a", instances: 1}, {uid: "b", instances: 1}}, nil
	}
	in := []retrieveTarget{studyTarget("1.1"), studyTarget("1.2"), studyTarget("1.3")}
	out := expandStudyTargets(ctx, in, 2, lister)
	if !reflect.DeepEqual(out, in) {
		t.Errorf("cancelled run changed the targets: %+v", out)
	}
	if calls.Load() != 0 {
		t.Errorf("cancelled run listed %d studies", calls.Load())
	}
}

// A list that arrives after the run was cancelled is not used either.
func TestExpandStudyTargetsIgnoresListAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	lister := func(context.Context, string, string) ([]retrieveSeries, error) {
		cancel()
		return []retrieveSeries{{uid: "a", instances: 1}, {uid: "b", instances: 1}}, nil
	}
	in := []retrieveTarget{studyTarget("1.1")}
	if out := expandStudyTargets(ctx, in, 2, lister); !reflect.DeepEqual(out, in) {
		t.Errorf("list after cancel was used: %+v", out)
	}
}

func TestRetrieveWeights(t *testing.T) {
	counted := []retrieveTarget{{instances: 10}, {instances: 3}, {instances: 1}}
	if got := retrieveWeights(counted); !reflect.DeepEqual(got, []float64{10, 3, 1}) {
		t.Errorf("counted: %v", got)
	}
	mixed := []retrieveTarget{{instances: 10}, {level: "STUDY"}, {instances: 1}}
	if got := retrieveWeights(mixed); !reflect.DeepEqual(got, []float64{1, 1, 1}) {
		t.Errorf("mixed: %v, want equal shares", got)
	}
	if got := retrieveProgress([]float64{1, 0, 0}, []float64{10, 3, 1}); got != 10.0/14 {
		t.Errorf("weighted progress %v, want %v", got, 10.0/14)
	}
	if got := retrieveProgress([]float64{1, 0, 0}, []float64{1, 1, 1}); got != 1.0/3 {
		t.Errorf("equal progress %v, want %v", got, 1.0/3)
	}
	if got := retrieveProgress(nil, nil); got != 0 {
		t.Errorf("empty progress %v", got)
	}
}

// One selected study of four series against the loopback PACS with three
// associations allowed: the study is listed, split into its series, and those
// are fetched three at a time, every file arriving once.
func TestStudySplitsIntoSeriesAgainstFakePACS(t *testing.T) {
	const scpPort = 11199
	received := startTestSCP(t, "TESTSCU", scpPort)
	pacs := newFakePACSSeries(t, 1, 4, 2, 0, map[string]string{"TESTSCU": fmt.Sprintf("127.0.0.1:%d", scpPort)})
	host, port := pacs.addr()
	cl := NewDicomClient(ServerProfile{Host: host, Port: port, RemoteAETitle: "FAKEPACS"}, "TESTSCU")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	study := pacs.studyUIDs()[0]
	targets := expandStudyTargets(ctx, []retrieveTarget{{level: "STUDY", studyUID: study}}, 3, cl.listSeries)
	if len(targets) != 4 {
		t.Fatalf("%d targets after the split, want 4: %+v", len(targets), targets)
	}
	for _, tg := range targets {
		if tg.level != "SERIES" || tg.studyUID != study || tg.seriesUID == "" || tg.instances != 2 {
			t.Fatalf("unexpected target %+v", tg)
		}
	}
	if pacs.finds.Load() != 1 {
		t.Errorf("%d series listings for one study", pacs.finds.Load())
	}

	sched := newRetrieveRun(len(targets), 3)
	sched.run(ctx, func(tctx context.Context, i int) error {
		tg := targets[i]
		return cl.Move(tctx, tg.level, tg.patientID, tg.studyUID, tg.seriesUID, "TESTSCU", nil)
	})
	if ctx.Err() != nil {
		t.Fatal("retrieve timed out")
	}
	assertAllDone(t, sched)
	if got := pacs.peak.Load(); got != 3 {
		t.Errorf("peak concurrent moves %d, want 3", got)
	}
	if n := waitForCount(received, 8); n != 8 {
		t.Errorf("SCP received %d files, want 8", n)
	}
}
