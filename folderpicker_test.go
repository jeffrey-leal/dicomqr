package main

// The picker itself is a modal Windows dialog and cannot be driven from a
// test. What can be checked is the machinery around it: the string conversion
// that turns COM's UTF-16 result into a path, and that a start folder resolves
// to a shell item (or safely to nil), which is what decides whether the dialog
// opens where the user last was.

import (
	"runtime"
	"syscall"
	"testing"
	"unsafe"
)

func TestUTF16PtrToString(t *testing.T) {
	for _, want := range []string{
		"",
		`C:\Users\jeffr\DICOM Downloads`,
		`\\server\share\studies`,
		"C:\\Ärzte\\Röntgen", // non-ASCII survives the round trip
	} {
		p, err := syscall.UTF16PtrFromString(want)
		if err != nil {
			t.Fatalf("UTF16PtrFromString(%q): %v", want, err)
		}
		if got := utf16PtrToString(p); got != want {
			t.Errorf("utf16PtrToString = %q, want %q", got, want)
		}
	}
	// A null result must read as empty rather than panicking: COM returns one
	// on any failure path, and the caller checks the string.
	if got := utf16PtrToString(nil); got != "" {
		t.Errorf("utf16PtrToString(nil) = %q, want empty", got)
	}
}

// A start folder is a convenience, never a precondition: an unusable one must
// yield nil so the dialog simply opens where Windows would have, rather than
// failing the whole picker.
//
// COM is initialised here for the same reason browseFolderNative does it
// around its own call — SHCreateItemFromParsingName is a shell COM call and
// fails outright without an apartment on the calling thread.
func TestShellItemFromPath(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	hr, _, _ := procCoInitializeEx.Call(0, uintptr(coinitApartmentThreaded|coinitDisableOLE1DDE))
	switch uint32(hr) {
	case 0, hrFalse:
		defer procCoUninitialize.Call()
	case hrChangedMode:
	default:
		t.Skipf("COM unavailable on this machine (CoInitializeEx = %#x)", uint32(hr))
	}

	dir := t.TempDir()
	item := shellItemFromPath(dir)
	if item == nil {
		t.Fatalf("shellItemFromPath(%q) = nil, want a shell item for a real folder", dir)
	}
	comRelease(item)

	for _, bad := range []string{
		"",
		string([]byte{0}), // embedded NUL: UTF16PtrFromString rejects it
		`Z:\no\such\folder\on\this\machine`,
	} {
		if got := shellItemFromPath(bad); got != nil {
			comRelease(got)
			t.Errorf("shellItemFromPath(%q) returned an item, want nil", bad)
		}
	}
}

// The vtable slots are positions in a C++ interface, so every declared
// method occupies one — including the getters this code never calls. A
// miscount neither fails to compile nor fails to run: it calls a different
// method with another one's arguments. That happened: GetResult was off by
// three (GetFolder, GetCurrentSelection and GetFileName were missed), so it
// invoked SetTitle with the address of the result pointer, returned success,
// and left the result nil — the picker opened, the user chose a folder, and
// nothing came back.
//
// The interface's declared order is written out here once, and the constants
// are checked against it. Sources: IModalWindow and IFileDialog /
// IFileOpenDialog in shobjidl_core.h, IShellItem in shobjidl_core.h.
func TestCOMVtableIndices(t *testing.T) {
	// IUnknown, then IModalWindow, then IFileDialog's own methods, in order.
	fileDialog := []string{
		"QueryInterface", "AddRef", "Release", // IUnknown
		"Show", // IModalWindow
		"SetFileTypes", "SetFileTypeIndex", "GetFileTypeIndex",
		"Advise", "Unadvise",
		"SetOptions", "GetOptions",
		"SetDefaultFolder", "SetFolder", "GetFolder", "GetCurrentSelection",
		"SetFileName", "GetFileName",
		"SetTitle", "SetOkButtonLabel", "SetFileNameLabel",
		"GetResult",
		"AddPlace", "SetDefaultExtension", "Close",
		"SetClientGuid", "ClearClientData", "SetFilter",
	}
	shellItem := []string{
		"QueryInterface", "AddRef", "Release", // IUnknown
		"BindToHandler", "GetParent", "GetDisplayName", "GetAttributes", "Compare",
	}

	indexOf := func(methods []string, name string) int {
		for i, m := range methods {
			if m == name {
				return i
			}
		}
		return -1
	}
	for _, tc := range []struct {
		iface   string
		methods []string
		name    string
		got     int
	}{
		{"IUnknown", fileDialog, "Release", vtRelease},
		{"IModalWindow", fileDialog, "Show", vtShow},
		{"IFileDialog", fileDialog, "SetOptions", vtSetOptions},
		{"IFileDialog", fileDialog, "GetOptions", vtGetOptions},
		{"IFileDialog", fileDialog, "SetFolder", vtSetFolder},
		{"IFileDialog", fileDialog, "SetTitle", vtSetTitle},
		{"IFileDialog", fileDialog, "GetResult", vtGetResult},
		{"IShellItem", shellItem, "GetDisplayName", vtItemGetDisplayName},
	} {
		if want := indexOf(tc.methods, tc.name); tc.got != want {
			t.Errorf("%s::%s is vtable slot %d, but the constant says %d",
				tc.iface, tc.name, want, tc.got)
		}
	}
}

// comCall reaches a method through the object's vtable pointer. A hand-built
// object proves the indexing without a real COM server: slot N of the vtable
// is what gets called, with the object as the first argument.
func TestComCallDispatchesThroughVtable(t *testing.T) {
	var gotSelf uintptr
	var gotArg uintptr
	stub := syscall.NewCallback(func(self, arg uintptr) uintptr {
		gotSelf, gotArg = self, arg
		return 7
	})

	// An object is a pointer to a vtable pointer; slot 3 is the one called.
	vtbl := [32]uintptr{}
	vtbl[3] = stub
	vtblPtr := unsafe.Pointer(&vtbl)
	obj := unsafe.Pointer(&vtblPtr)

	if ret := comCall(obj, 3, 42); ret != 7 {
		t.Errorf("comCall returned %d, want the stub's 7", ret)
	}
	if gotSelf != uintptr(obj) {
		t.Errorf("stub received self = %#x, want the object pointer %#x", gotSelf, uintptr(obj))
	}
	if gotArg != 42 {
		t.Errorf("stub received arg = %d, want 42", gotArg)
	}
}
