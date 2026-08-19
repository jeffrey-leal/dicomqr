package main

// How much memory a modification run may hold at once.
//
// The figure is derived from the machine rather than fixed, because the range
// this application runs on is wide: a reporting laptop with 8 GB and a
// workstation with 128 GB should not export under the same cap. One constant
// would be either wasteful on the large machine or dangerous on the small one.
//
// Windows is asked directly through the same syscall route windowplacement.go
// uses for user32 and nvthreadctl.go for NVAPI. Every step is best-effort: a
// failure anywhere falls back to a fixed default, because a run that cannot
// size its budget should still run.

import (
	"os"
	"strconv"
	"unsafe"
)

const (
	// modifyMemoryShare of physical memory is what a run may hold. A quarter
	// leaves the viewer, the catalog, the OS file cache and whatever else the
	// user has open with room to work — an export is a foreground task, not the
	// only thing on the machine.
	modifyMemoryShare = 4

	// The floor keeps a small or virtualised machine making progress; a single
	// file larger than the whole budget is clamped to it by memBudget.acquire
	// and runs alone, so a low budget slows a run down rather than stalling it.
	//
	// Above the ceiling the four-worker cap is the binding constraint anyway —
	// a larger budget simply means the gate never engages, which is the right
	// behaviour on a large workstation — so raising it further would buy
	// nothing while giving the application licence to make a machine swap.
	modifyMemoryFloor   = 512 << 20
	modifyMemoryCeiling = 4 << 30

	// Used when the machine cannot be asked. Deliberately at the floor rather
	// than optimistic: an unknown machine is the one least worth gambling on.
	modifyMemoryFallback = modifyMemoryFloor
)

// modifyMemoryBudget is the byte ceiling on a run's in-flight decompressed
// pixel data, resolved once at startup. A var, like clipBufferBudget, so tests
// can shrink it far below any real file and drive the gating deterministically.
var modifyMemoryBudget = resolveModifyMemoryBudget()

// resolveModifyMemoryBudget picks the budget: the environment override if it is
// set and sane, otherwise a share of physical memory, otherwise the fallback.
func resolveModifyMemoryBudget() int64 {
	if v := os.Getenv("DICOMQR_MODIFY_BUDGET_MB"); v != "" {
		if mb, err := strconv.ParseInt(v, 10, 64); err == nil && mb > 0 {
			// An explicit override is honoured as given — no clamping. Someone
			// setting this is diagnosing something and means the number.
			return mb << 20
		}
	}
	total := totalPhysicalMemory()
	if total <= 0 {
		return modifyMemoryFallback
	}
	return clampInt64(total/modifyMemoryShare, modifyMemoryFloor, modifyMemoryCeiling)
}

func clampInt64(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// memoryStatusEx mirrors Windows MEMORYSTATUSEX: two DWORDs followed by seven
// DWORDLONGs, 64 bytes in total. Length must carry that size before the call.
type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

// kernel32 itself is declared in windowplacement.go, which loads it for
// GetCurrentProcessId — one handle per DLL across the package.
var procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")

// totalPhysicalMemory returns the machine's installed RAM in bytes, or 0 when
// Windows cannot be asked.
//
// TotalPhys rather than AvailPhys deliberately: available memory swings with
// whatever else happens to be running, so a budget derived from it would differ
// between two runs of the same export and could collapse to almost nothing on a
// busy machine. Installed memory is a property of the hardware, which is what a
// stable cap should follow.
func totalPhysicalMemory() int64 {
	if err := procGlobalMemoryStatusEx.Find(); err != nil {
		return 0
	}
	st := memoryStatusEx{}
	st.Length = uint32(unsafe.Sizeof(st))
	ret, _, _ := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&st)))
	if ret == 0 {
		return 0
	}
	if st.TotalPhys > 1<<62 { // implausible; treat as unreadable rather than overflow int64
		return 0
	}
	return int64(st.TotalPhys)
}
