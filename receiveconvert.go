package main

// Received-file conversion, after the PACS has had its reply.
//
// When a server profile requires a transfer syntax, a received object in any
// other syntax has to be converted before it reaches the download folder. That
// used to happen inside the C-STORE handler, before its response: the PACS
// waits for each response before sending the next object, so every conversion
// — a full parse, every frame decoded, the file rewritten — sat on the
// transfer's critical path, and a retrieve of compressed data ran at the speed
// of one decoder.
//
// Now the handler writes what arrived to a pending file, replies Success, and
// the conversion happens here, in the background: on the same bounded pool the
// modification engine uses (CPU tokens sized to half the logical processors,
// the machine's memory budget, below-normal priority — see modifyWorkerCount,
// frameparallel.go, workerpriority.go), so a multi-frame file also spreads
// across idle cores.
//
// The trade-off, accepted deliberately (user decision, 2026-09-30): a
// conversion that fails, or a folder that cannot be created afterwards, can no
// longer be reported to the PACS as a failed sub-operation — it has already
// been told Success. Such a file is counted and logged locally instead, and
// shows in the retrieve's summary. An object that needs no conversion is still
// finished before the reply, so its errors still reach the PACS.
//
// Nothing received is lost to a crash or a quit mid-conversion: the pending
// file is named after the syntax it must become (.convert_<UID>_*.tmp), and
// recoverPendingConversions finishes whatever an earlier session left.

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
)

// receiveOutcome is what finishing a received object came to.
type receiveOutcome int

const (
	receiveSaved          receiveOutcome = iota // placed in the download folder
	receiveAlreadyPresent                       // an acceptable copy was already there; discarded
	receiveSkipped                              // could not be converted; discarded (logged)
)

type receiveResult struct {
	dest      string
	outcome   receiveOutcome
	converted bool
}

// finishReceivedFile moves one completely written received object — File Meta
// plus data set, at tmpPath inside downloadDir — to its place in the organized
// hierarchy, converting it to requiredTS first when it is in another syntax.
// tmpPath is consumed on every path: renamed into place or removed.
//
// A copy already at the destination wins unless a syntax is required and the
// copy is in a different one (downloaded before the requirement was set), in
// which case it is replaced. An object that cannot be converted is skipped:
// logged, nothing saved, no error — the retrieve carries on. label prefixes
// the log lines ("scp", "c-get", "recover").
func finishReceivedFile(tmpPath, downloadDir, sopInstanceUID, sopClassUID, fileTS, requiredTS, label string,
	tokens cpuTokens) (receiveResult, error) {

	patientName, patientID, studyDesc, studyDate, seriesDesc, seriesNumber := scpParseMetadata(tmpPath)
	dest := organizeFilePath(downloadDir, patientName, patientID, studyDesc, studyDate, seriesDesc, seriesNumber, sopInstanceUID)

	if _, statErr := os.Stat(dest); statErr == nil {
		if requiredTS == "" || fileTransferSyntaxUID(dest) == requiredTS {
			os.Remove(tmpPath)
			return receiveResult{dest: dest, outcome: receiveAlreadyPresent}, nil
		}
	}

	// Enforce the required syntax before the file reaches its destination, so
	// the download folder only ever holds conforming files.
	converted := false
	if requiredTS != "" && fileTS != requiredTS {
		changed, convErr := transcodeDICOMFileTokens(tmpPath, requiredTS, tokens)
		if convErr != nil {
			logWarn("%s: SKIPPED %s — cannot convert to %s: %v (series %q, SOP class %s); object not saved, retrieve continues",
				label, sopInstanceUID, transferSyntaxLabel(requiredTS), convErr, seriesDesc, sopClassUID)
			os.Remove(tmpPath)
			return receiveResult{outcome: receiveSkipped}, nil
		}
		converted = changed
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		os.Remove(tmpPath)
		return receiveResult{}, err
	}
	// Rename is atomic on the same volume (the temp file lives in downloadDir
	// for exactly this reason); copy+delete is the fallback.
	if err := os.Rename(tmpPath, dest); err != nil {
		if copyErr := scpCopyFile(tmpPath, dest); copyErr != nil {
			os.Remove(tmpPath)
			return receiveResult{}, copyErr
		}
		os.Remove(tmpPath)
	}
	return receiveResult{dest: dest, outcome: receiveSaved, converted: converted}, nil
}

// pendingConvertPrefix names a received object queued for conversion; the
// required syntax follows it, so recovery knows what to convert to without any
// state beyond the file itself.
const pendingConvertPrefix = ".convert_"

// markPendingConversion renames a received temp file to its pending name —
// .convert_<requiredTS>_<unique>.tmp in the same folder — and returns it.
func markPendingConversion(tmpPath, requiredTS string) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(tmpPath), pendingConvertPrefix+requiredTS+"_*.tmp")
	if err != nil {
		return "", err
	}
	name := f.Name()
	f.Close()
	if err := os.Rename(tmpPath, name); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

// pendingConvertTarget reads the required syntax back out of a pending name.
func pendingConvertTarget(name string) (string, bool) {
	base := filepath.Base(name)
	if !strings.HasPrefix(base, pendingConvertPrefix) || !strings.HasSuffix(base, ".tmp") {
		return "", false
	}
	rest := strings.TrimPrefix(base, pendingConvertPrefix)
	i := strings.LastIndex(rest, "_")
	if i <= 0 {
		return "", false
	}
	return rest[:i], true
}

// receiveConverter runs conversions of received objects in the background.
// Jobs queue without bound — each is a file already safely on disk, so the
// queue costs disk, not memory — and run on at most the CPU allowance of
// workers at once, each admitted by the memory budget and holding a CPU token.
type receiveConverter struct {
	tokens     cpuTokens
	budget     *memBudget
	maxWorkers int

	mu      sync.Mutex
	queue   []convertJob
	running int

	pending sync.WaitGroup
	count   atomic.Int64
}

type convertJob struct {
	weight int64
	run    func(tokens cpuTokens)
}

func newReceiveConverter() *receiveConverter {
	allowance := modifyWorkerCount(true, math.MaxInt32)
	return &receiveConverter{
		tokens:     newCPUTokens(allowance),
		budget:     newMemBudget(modifyMemoryBudget),
		maxWorkers: allowance,
	}
}

// submit queues a conversion. weight is its memory reservation (see
// fileMemoryWeight). Workers start on demand and exit when the queue drains,
// so an idle converter holds no threads.
func (c *receiveConverter) submit(weight int64, run func(tokens cpuTokens)) {
	c.pending.Add(1)
	c.count.Add(1)
	c.mu.Lock()
	c.queue = append(c.queue, convertJob{weight: weight, run: run})
	start := c.running < c.maxWorkers
	if start {
		c.running++
	}
	c.mu.Unlock()
	if start {
		go c.work()
	}
}

func (c *receiveConverter) work() {
	defer lowerWorkerPriority()()
	for {
		c.mu.Lock()
		if len(c.queue) == 0 {
			c.running--
			c.mu.Unlock()
			return
		}
		job := c.queue[0]
		c.queue = c.queue[1:]
		c.mu.Unlock()
		c.runJob(job)
	}
}

func (c *receiveConverter) runJob(job convertJob) {
	defer c.pending.Done()
	defer c.count.Add(-1)
	// A panicking conversion must not end the process, and its pending file is
	// left for recovery at the next start rather than lost.
	defer func() {
		if r := recover(); r != nil {
			logError("receive: PANIC converting a received object: %v\n%s", r, debug.Stack())
		}
	}()
	defer c.budget.acquire(job.weight)()
	defer c.tokens.hold()()
	job.run(c.tokens)
}

// wait blocks until every conversion submitted so far has finished.
func (c *receiveConverter) wait() { c.pending.Wait() }

// pendingCount is how many conversions are queued or running.
func (c *receiveConverter) pendingCount() int64 { return c.count.Load() }

// recoverPendingConversions finishes objects an earlier session received but
// did not get to convert — the app was closed or crashed mid-retrieve — so
// nothing the PACS was told had arrived is lost. Each is converted to the
// syntax its name records and placed in the hierarchy; onSaved is called (on a
// converter worker, possibly concurrently) for every file that lands. Returns
// how many were found.
func recoverPendingConversions(dir string, conv *receiveConverter, onSaved func(dest string)) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	found := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		target, ok := pendingConvertTarget(e.Name())
		if !ok {
			continue
		}
		path := filepath.Join(dir, e.Name())
		id, err := fileMetaIdentity(path)
		if err != nil {
			// Not a complete object — it cannot be finished, and would
			// otherwise sit in the folder for ever.
			logWarn("recover: discarding unreadable pending file %s: %v", e.Name(), err)
			os.Remove(path)
			continue
		}
		found++
		conv.submit(fileMemoryWeight(path), func(tokens cpuTokens) {
			res, err := finishReceivedFile(path, dir, id.sopInstanceUID, id.sopClassUID,
				id.transferSyntaxUID, target, "recover", tokens)
			switch {
			case err != nil:
				logError("recover: could not save %s: %v", id.sopInstanceUID, err)
			case res.outcome == receiveSaved:
				onSaved(res.dest)
			}
		})
	}
	if found > 0 {
		logInfo("recover: finishing %d received file(s) whose conversion an earlier session did not complete", found)
	}
	return found
}

// describeLocalFailures is the summary clause for received objects the PACS was
// told had arrived but that could not be saved after conversion.
func describeLocalFailures(n int64) string {
	return fmt.Sprintf(" — %d received file(s) could not be saved after conversion, see Activity Log", n)
}
