package main

// Folder picking, owned by the window that asked for it.
//
// github.com/sqweek/dialog builds its BROWSEINFO (and OPENFILENAME) with
// hwndOwner = 0, so the dialog it opens owns nothing and nothing owns it. Two
// consequences, both field-reported: it can appear *behind* the application
// window — the application calls it from a goroutine, which is never the
// thread holding the GLFW window, and a thread without foreground rights
// cannot raise an unowned window — and it is not kept above the window it
// belongs to afterwards.
//
// The fix is the owner handle, so this asks Windows directly, the same
// syscall route windowplacement.go and nvthreadctl.go already take. While
// writing it anyway, it uses IFileDialog with FOS_PICKFOLDERS — the modern
// picker, with a breadcrumb bar, a places list, a type-able path and a
// resizable window — rather than replicating SHBrowseForFolder's tree.
//
// Every failure falls back to sqweek's picker, so a machine where the COM
// path does not work still gets a folder chooser rather than nothing.

import (
	"runtime"
	"syscall"
	"unsafe"

	sqweekdialog "github.com/sqweek/dialog"
)

var (
	ole32                = syscall.NewLazyDLL("ole32.dll")
	procCoInitializeEx   = ole32.NewProc("CoInitializeEx")
	procCoUninitialize   = ole32.NewProc("CoUninitialize")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")
	procCoTaskMemFree    = ole32.NewProc("CoTaskMemFree")

	shell32                     = syscall.NewLazyDLL("shell32.dll")
	procSHCreateItemFromParsing = shell32.NewProc("SHCreateItemFromParsingName")

	// user32 is declared in windowplacement.go, which owns the window handles
	// this picker parents to.
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
)

// comGUID is the Windows GUID layout.
type comGUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var (
	clsidFileOpenDialog = comGUID{0xDC1C5A9C, 0xE88A, 0x4DDE, [8]byte{0xA5, 0xA1, 0x60, 0xF8, 0x2A, 0x20, 0xAE, 0xF7}}
	iidIFileOpenDialog  = comGUID{0xD57C7288, 0xD4AD, 0x4768, [8]byte{0xBE, 0x02, 0x9D, 0x96, 0x95, 0x32, 0xD9, 0x60}}
	iidIShellItem       = comGUID{0x43826D1E, 0xE718, 0x42EE, [8]byte{0xBC, 0x55, 0xA1, 0xE2, 0x61, 0xC3, 0x7B, 0xFE}}
)

const (
	coinitApartmentThreaded = 0x2
	coinitDisableOLE1DDE    = 0x4
	clsctxInprocServer      = 0x1

	// IFileDialog options: folders only, filesystem paths only, and the path
	// must exist — a picker that can return a namespace object or a
	// non-existent path would hand the rest of the application something it
	// cannot open.
	fosPickFolders     = 0x00000020
	fosForceFileSystem = 0x00000040
	fosPathMustExist   = 0x00000800
	sigdnFileSysPath   = 0x80058000
	hrCancelled        = 0x800704C7 // HRESULT_FROM_WIN32(ERROR_CANCELLED)
	hrChangedMode      = 0x80010106 // RPC_E_CHANGED_MODE
	hrFalse            = 0x00000001 // S_FALSE — already initialised on this thread
)

// IFileDialog / IShellItem vtable slots, in interface declaration order
// (IUnknown, then IModalWindow, then the interface's own methods).
//
// These are positions in a C++ vtable, so every method counts — including the
// getters this code never calls. Miscounting does not fail to compile and does
// not fail to run: it calls a *different* method with the arguments meant for
// another. Getting GetResult wrong called SetTitle with the address of the
// result pointer, which returned success while leaving the result nil, so the
// dialog worked and the chosen folder silently never arrived. The full list
// lives in folderpicker_test.go, which derives these indices and fails if one
// drifts.
const (
	vtRelease = 2
	vtShow    = 3 // IModalWindow::Show

	vtSetOptions = 9  // IFileDialog::SetOptions
	vtGetOptions = 10 // IFileDialog::GetOptions
	vtSetFolder  = 12 // IFileDialog::SetFolder
	vtSetTitle   = 17 // IFileDialog::SetTitle
	vtGetResult  = 20 // IFileDialog::GetResult

	vtItemGetDisplayName = 5 // IShellItem::GetDisplayName
)

// comCall invokes the method at vtable slot idx on a COM object.
//
// COM objects are held as unsafe.Pointer rather than uintptr throughout: a
// uintptr is a number, not a reference, and reconstituting a pointer from one
// is exactly what `go vet`'s unsafeptr check exists to catch. Converting the
// other way — pointer to uintptr, inside the call expression — is the
// sanctioned direction.
func comCall(obj unsafe.Pointer, idx int, args ...uintptr) uintptr {
	vtbl := (*[32]uintptr)(*(*unsafe.Pointer)(obj))
	ret, _, _ := syscall.SyscallN(vtbl[idx], append([]uintptr{uintptr(obj)}, args...)...)
	return ret
}

func comRelease(obj unsafe.Pointer) {
	if obj != nil {
		comCall(obj, vtRelease)
	}
}

// browseFolder opens a folder chooser owned by the application window titled
// ownerTitle, starting at startDir, and returns the chosen path. ok is false
// when the user cancelled.
//
// Callers pass a window title rather than a handle because Fyne never exposes
// the native handle (see windowplacement.go); findProcessWindow resolves it.
// An unknown or unmatched title still opens a picker, just an unowned one.
func browseFolder(ownerTitle, title, startDir string) (string, bool) {
	owner, _ := findProcessWindow(ownerTitle)
	if path, ok, handled := browseFolderNative(owner, title, startDir); handled {
		return path, ok
	}
	// COM unavailable or refused: the old picker is better than none.
	picker := sqweekdialog.Directory()
	if title != "" {
		picker = picker.Title(title)
	}
	if startDir != "" {
		picker = picker.SetStartDir(startDir)
	}
	path, err := picker.Browse()
	return path, err == nil
}

// browseFolderNative runs the IFileDialog picker. handled is false when the
// COM path could not be taken at all, which is the caller's signal to fall
// back; a handled call reports the user's choice in (path, ok).
func browseFolderNative(owner uintptr, title, startDir string) (path string, ok, handled bool) {
	// COM apartments are per-thread, so the goroutine must stay on the thread
	// it initialises — and the modal loop below runs on it too.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hr, _, _ := procCoInitializeEx.Call(0, uintptr(coinitApartmentThreaded|coinitDisableOLE1DDE))
	switch uint32(hr) {
	case 0, hrFalse:
		defer procCoUninitialize.Call()
	case hrChangedMode:
		// Already initialised in another mode by something else on this
		// thread; usable, but this call did not own it, so it must not
		// uninitialise it.
	default:
		return "", false, false
	}

	var dialog unsafe.Pointer
	hr, _, _ = procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidFileOpenDialog)), 0, clsctxInprocServer,
		uintptr(unsafe.Pointer(&iidIFileOpenDialog)), uintptr(unsafe.Pointer(&dialog)))
	if uint32(hr) != 0 || dialog == nil {
		return "", false, false
	}
	defer comRelease(dialog)

	// Add to the existing options rather than replacing them: the defaults
	// carry behaviour (such as restoring the last-used size) worth keeping.
	var opts uint32
	if ret := comCall(dialog, vtGetOptions, uintptr(unsafe.Pointer(&opts))); uint32(ret) != 0 {
		return "", false, false
	}
	if hr := comCall(dialog, vtSetOptions,
		uintptr(opts|fosPickFolders|fosForceFileSystem|fosPathMustExist)); uint32(hr) != 0 {
		return "", false, false
	}
	if title != "" {
		if p, err := syscall.UTF16PtrFromString(title); err == nil {
			comCall(dialog, vtSetTitle, uintptr(unsafe.Pointer(p)))
		}
	}
	if startDir != "" {
		// A start folder that no longer exists is not an error: the picker
		// simply opens wherever Windows would have.
		if item := shellItemFromPath(startDir); item != nil {
			comCall(dialog, vtSetFolder, uintptr(item))
			comRelease(item)
		}
	}

	// The owner is the whole point: with it the dialog is modal to the
	// application window and can never be lost behind it. Raising the owner
	// first covers the case where the application itself is not foreground,
	// so the dialog does not open behind an unrelated window either.
	if owner != 0 {
		procSetForegroundWindow.Call(owner)
	}
	hr = comCall(dialog, vtShow, owner)
	if uint32(hr) == hrCancelled {
		return "", false, true
	}
	if uint32(hr) != 0 {
		logWarn("folder picker: IFileDialog::Show failed (%#x) — falling back to the basic chooser", uint32(hr))
		return "", false, false
	}

	// Past Show, the user has chosen a folder and expects it to arrive. Any
	// failure from here is logged rather than swallowed: a picker that closes
	// and does nothing is indistinguishable from one that was cancelled, which
	// is exactly how a wrong vtable index went unnoticed.
	var item unsafe.Pointer
	if hr := comCall(dialog, vtGetResult, uintptr(unsafe.Pointer(&item))); uint32(hr) != 0 || item == nil {
		logWarn("folder picker: a folder was chosen but GetResult returned nothing (%#x)", uint32(hr))
		return "", false, false
	}
	defer comRelease(item)

	var wpath *uint16
	if hr := comCall(item, vtItemGetDisplayName, sigdnFileSysPath, uintptr(unsafe.Pointer(&wpath))); uint32(hr) != 0 || wpath == nil {
		logWarn("folder picker: the chosen item has no filesystem path (%#x)", uint32(hr))
		return "", false, false
	}
	defer procCoTaskMemFree.Call(uintptr(unsafe.Pointer(wpath)))

	path = utf16PtrToString(wpath)
	if path == "" {
		logWarn("folder picker: the chosen item resolved to an empty path")
		return "", false, false
	}
	return path, true, true
}

// shellItemFromPath resolves a filesystem path to an IShellItem, or nil.
//
// Requires an initialised COM apartment on the calling thread — it is a shell
// COM call, and without one it simply fails. browseFolderNative is inside its
// own CoInitializeEx scope when it calls this.
func shellItemFromPath(path string) unsafe.Pointer {
	// Windows resolves an empty parsing name to a real shell folder rather
	// than failing, which would silently override the picker's own idea of
	// where to open. "No start folder" has to mean exactly that.
	if path == "" {
		return nil
	}
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil
	}
	var item unsafe.Pointer
	hr, _, _ := procSHCreateItemFromParsing.Call(
		uintptr(unsafe.Pointer(p)), 0,
		uintptr(unsafe.Pointer(&iidIShellItem)), uintptr(unsafe.Pointer(&item)))
	if uint32(hr) != 0 {
		return nil
	}
	return item
}

// utf16PtrToString reads a NUL-terminated UTF-16 string returned by COM.
// syscall.UTF16ToString needs a slice, and the length is not known until the
// terminator is found — walked with unsafe.Add rather than arithmetic on a
// uintptr, which would be a pointer the garbage collector cannot see.
func utf16PtrToString(p *uint16) string {
	if p == nil {
		return ""
	}
	n := 0
	for ptr := unsafe.Pointer(p); *(*uint16)(ptr) != 0; ptr = unsafe.Add(ptr, 2) {
		n++
	}
	return syscall.UTF16ToString(unsafe.Slice(p, n))
}
