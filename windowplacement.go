package main

// Initial placement of the main window.
//
// Fyne cannot do this on its own: fyne.Window exposes Resize and
// CenterOnScreen and nothing else — no position, no move — and there is no API
// for the screen dimensions either. Placing the window anywhere except dead
// centre therefore means asking Windows directly, which this file does through
// the same syscall route nvthreadctl.go uses for NVAPI.
//
// Everything here works in physical pixels. Fyne's Resize takes scaled
// (logical) units, so mixing the two would misplace the window on a display at
// anything other than 100%; SetWindowPos sets size and position together, in
// the same units the work area is measured in, and Fyne picks the change up as
// an ordinary resize event.
//
// Every step is best-effort. A failure anywhere leaves the window wherever
// Fyne and the window manager would have put it, which is a cosmetic loss, so
// nothing here is fatal and nothing blocks startup.

import (
	"syscall"
	"time"
	"unsafe"
)

const (
	spiGetWorkArea = 0x0030

	swpNoZOrder   = 0x0004
	swpNoActivate = 0x0010
	swpNoSize     = 0x0001

	// Share of the leftover width placed to the left of the window. Below a
	// half, so the window sits left of centre as intended; far enough from
	// zero that it does not look pinned to the edge.
	mainWindowLeftBias = 0.25

	// How long to keep looking for the native window after the app reports
	// itself started. The handle appears as soon as the window is mapped,
	// normally on the first attempt.
	mainWindowFindTimeout = 3 * time.Second
)

type winRect struct {
	Left, Top, Right, Bottom int32
}

var (
	user32                   = syscall.NewLazyDLL("user32.dll")
	procSystemParametersInfo = user32.NewProc("SystemParametersInfoW")
	procEnumWindows          = user32.NewProc("EnumWindows")
	procGetWindowThreadPID   = user32.NewProc("GetWindowThreadProcessId")
	procIsWindowVisible      = user32.NewProc("IsWindowVisible")
	procGetWindowTextW       = user32.NewProc("GetWindowTextW")
	procSetWindowPos         = user32.NewProc("SetWindowPos")
	procGetWindowRect        = user32.NewProc("GetWindowRect")

	kernel32                = syscall.NewLazyDLL("kernel32.dll")
	procGetCurrentProcessID = kernel32.NewProc("GetCurrentProcessId")
)

// winPoint is a saved window origin, in physical pixels.
type winPoint struct{ X, Y int32 }

// mainWindowPlacement decides the window's rectangle. The size is a share of
// the work area when sizeIt is set, otherwise the size the window already has
// — never an assumed one, because centring a window on a height it does not
// have is what previously pushed its status bar under the taskbar. The origin
// is the saved one when the user has placed the window before, otherwise the
// default: vertically centred and biased left.
//
// The clamp at the end is the guarantee that matters: whatever the inputs —
// a size larger than the screen, a saved position from a monitor that has
// since been unplugged, a taskbar that has moved — the window ends up wholly
// inside the work area. Kept free of any Windows call so it can be tested.
func mainWindowPlacement(area, cur winRect, saved *winPoint,
	widthFrac, heightFrac float64, sizeIt bool) (x, y, w, h int32) {

	areaW := area.Right - area.Left
	areaH := area.Bottom - area.Top

	if sizeIt {
		w = int32(float64(areaW) * widthFrac)
		h = int32(float64(areaH) * heightFrac)
	} else {
		w = cur.Right - cur.Left
		h = cur.Bottom - cur.Top
	}
	if w > areaW {
		w = areaW
	}
	if h > areaH {
		h = areaH
	}

	if saved != nil {
		x, y = saved.X, saved.Y
	} else {
		x = area.Left + int32(float64(areaW-w)*mainWindowLeftBias)
		y = area.Top + (areaH-h)/2
	}

	if x+w > area.Right {
		x = area.Right - w
	}
	if y+h > area.Bottom {
		y = area.Bottom - h
	}
	if x < area.Left {
		x = area.Left
	}
	if y < area.Top {
		y = area.Top
	}
	return x, y, w, h
}

// screenWorkArea returns the primary monitor's work area — the desktop minus
// the taskbar and any other appbars — in physical pixels. On a multi-monitor
// setup this is the primary display, which is where a freshly launched window
// lands anyway.
func screenWorkArea() (winRect, bool) {
	var r winRect
	ret, _, _ := procSystemParametersInfo.Call(
		uintptr(spiGetWorkArea), 0, uintptr(unsafe.Pointer(&r)), 0)
	if ret == 0 || r.Right <= r.Left || r.Bottom <= r.Top {
		return winRect{}, false
	}
	return r, true
}

// findProcessWindow returns the handle of a visible top-level window owned by
// this process whose title matches. Fyne keeps the GLFW window handle to
// itself, so the only way to reach it is to look for it.
func findProcessWindow(title string) (uintptr, bool) {
	selfPID, _, _ := procGetCurrentProcessID.Call()
	want := title

	var found uintptr
	cb := syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		var pid uint32
		procGetWindowThreadPID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
		if uintptr(pid) != selfPID {
			return 1 // keep enumerating
		}
		if visible, _, _ := procIsWindowVisible.Call(hwnd); visible == 0 {
			return 1
		}
		buf := make([]uint16, 256)
		n, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
		if n == 0 || syscall.UTF16ToString(buf[:n]) != want {
			return 1
		}
		found = hwnd
		return 0 // stop
	})
	procEnumWindows.Call(cb, 0)
	return found, found != 0
}

// mainWindowRect returns the outer rectangle of the main window, borders and
// title bar included, in physical pixels. That is what SetWindowPos consumes,
// so it is also what has to be stored to reproduce a placement later — the
// canvas size Fyne reports is the content area only, and is in scaled units.
func mainWindowRect(title string) (winRect, bool) {
	hwnd, ok := findProcessWindow(title)
	if !ok {
		return winRect{}, false
	}
	var r winRect
	ret, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	if ret == 0 || r.Right <= r.Left || r.Bottom <= r.Top {
		return winRect{}, false
	}
	return r, true
}

// placeMainWindow puts the main window where it should open: at the position
// saved from the last session, or failing that the default — widthFrac by
// heightFrac of the work area, centred vertically and biased left. sizeIt
// applies that computed size; without it the window keeps the size it already
// has, which is the one restored from settings.
//
// It runs on its own goroutine because the window may not be mapped the
// instant the app reports itself started, and polling must not hold up the UI.
func placeMainWindow(title string, saved *winPoint, widthFrac, heightFrac float64, sizeIt bool) {
	go func() {
		area, ok := screenWorkArea()
		if !ok {
			logWarn("window placement: work area unavailable; leaving the window where Windows put it")
			return
		}

		var hwnd uintptr
		deadline := time.Now().Add(mainWindowFindTimeout)
		for {
			if hwnd, ok = findProcessWindow(title); ok {
				break
			}
			if time.Now().After(deadline) {
				logWarn("window placement: main window not found within %s; leaving it where Windows put it",
					mainWindowFindTimeout)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}

		// The window's own rectangle, so a placement that is not resizing
		// centres and clamps against the height the window actually has.
		var cur winRect
		if ret, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&cur))); ret == 0 {
			logWarn("window placement: GetWindowRect failed; leaving the window where Windows put it")
			return
		}

		x, y, winW, winH := mainWindowPlacement(area, cur, saved, widthFrac, heightFrac, sizeIt)

		// Size is always supplied, even when it is the window's current size:
		// the clamp above may have had to shrink a window too large for the
		// work area, and SWP_NOSIZE would silently discard that.
		ret, _, err := procSetWindowPos.Call(hwnd, 0,
			uintptr(x), uintptr(y), uintptr(winW), uintptr(winH),
			uintptr(swpNoZOrder|swpNoActivate))
		if ret == 0 {
			logWarn("window placement: SetWindowPos failed: %v", err)
			return
		}
		logInfo("window placement: %dx%d at %d,%d (work area %dx%d at %d,%d, saved position %v)",
			winW, winH, x, y, area.Right-area.Left, area.Bottom-area.Top, area.Left, area.Top, saved != nil)
	}()
}
