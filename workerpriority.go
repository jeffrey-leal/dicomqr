package main

// Export workers run at below-normal CPU priority.
//
// A worker cap alone cannot make a batch job a good neighbour: whatever the
// number, those threads compete with the user's other applications — and with
// dicomqr's own UI — on equal terms, so a CPU-heavy export (zip compression,
// masking, decompression) makes the whole machine feel slow for as long as it
// runs. Scheduling priority is what expresses "use what is idle": on a quiet
// machine a below-normal thread runs exactly as fast as a normal one, and the
// moment anything at normal priority wants a core, Windows gives it that core
// first. That is what lets the worker count scale with the machine (see
// modifyWorkerCount) without the export ever crowding out the desktop.
//
// Only CPU priority is lowered. Windows' background mode
// (THREAD_MODE_BACKGROUND_BEGIN) would also drop the thread's disk and memory
// priority, and at very low I/O priority an export can stall behind any other
// process reading the same disk — a job the user is waiting on must not be
// starved, only polite.
//
// A goroutine is not a thread, so the worker first pins itself to its OS thread
// (runtime.LockOSThread); the lowered priority then applies to exactly that
// goroutine's work, cgo codec calls included, and to nothing else.

import "runtime"

var (
	procGetCurrentThread  = kernel32.NewProc("GetCurrentThread")
	procSetThreadPriority = kernel32.NewProc("SetThreadPriority")
	procGetThreadPriority = kernel32.NewProc("GetThreadPriority")
)

const (
	threadPriorityNormal      = 0
	threadPriorityBelowNormal = -1
)

// lowerWorkerPriority pins the calling goroutine to its OS thread and lowers
// that thread to below-normal priority, returning the function that undoes both.
// The caller defers it for the goroutine's lifetime.
//
// Undoing matters: an unlocked thread goes back to the runtime's pool and would
// carry the lowered priority onto whatever goroutine ran there next. So the
// restore puts the priority back before unlocking — and if that fails, leaves
// the thread locked, which makes the runtime discard it when the goroutine
// exits, so the priority still cannot leak. Any failure to lower in the first
// place just leaves the worker at normal priority, as it always was.
func lowerWorkerPriority() (restore func()) {
	if procSetThreadPriority.Find() != nil || procGetCurrentThread.Find() != nil {
		return func() {}
	}
	runtime.LockOSThread()
	thread, _, _ := procGetCurrentThread.Call() // a pseudo-handle: never closed
	if !setThreadPriority(thread, threadPriorityBelowNormal) {
		runtime.UnlockOSThread()
		return func() {}
	}
	return func() {
		if setThreadPriority(thread, threadPriorityNormal) {
			runtime.UnlockOSThread()
		}
	}
}

func setThreadPriority(thread uintptr, priority int32) bool {
	r, _, _ := procSetThreadPriority.Call(thread, uintptr(priority))
	return r != 0
}

// currentThreadPriority reports the calling OS thread's priority — for tests,
// which need to see the effect on the thread a worker is pinned to.
func currentThreadPriority() (int32, error) {
	if err := procGetThreadPriority.Find(); err != nil {
		return 0, err
	}
	thread, _, _ := procGetCurrentThread.Call()
	r, _, err := procGetThreadPriority.Call(thread)
	if int32(r) == 0x7FFFFFFF { // THREAD_PRIORITY_ERROR_RETURN
		return 0, err
	}
	return int32(r), nil
}
