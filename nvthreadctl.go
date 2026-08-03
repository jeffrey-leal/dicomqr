package main

// NVIDIA's "Threaded Optimization" driver feature corrupts multi-window OpenGL
// rendering: with two Fyne windows open and one repainting continuously (cine
// playback of an ultrasound chapter), the other window's content intermittently
// draws with garbage geometry — its text smeared into glyph-shaped streaks
// radiating across the window. Verified on an RTX A6000 with a minimal
// two-window Fyne app: Fyne 2.7.3, 2.7.4 and 2.8.0 all reproduce it, no GL
// error is ever reported, and every application-side serialisation tried
// (shared GL contexts, glFinish between windows, unconditional uniform sets)
// fails to prevent it. Turning the driver profile's Threaded Optimization off
// is the one change that stops it.
//
// disableNvidiaThreadedOptimization writes that setting for this executable
// programmatically through NVAPI's driver-settings (DRS) API — the same store
// NVIDIA Control Panel > Manage 3D Settings > Program Settings writes — so no
// user ever has to find that switch. The profile is persistent and re-checked
// (not re-written) on every launch. On systems without an NVIDIA driver there
// is no nvapi64.dll and this does nothing.
//
// NVAPI has no import library: functions are fetched from nvapi64.dll's single
// nvapi_QueryInterface export by well-known IDs. The IDs and struct layouts
// below match the public NVAPI headers; every struct carries a version word of
// (structSize | version<<16), which the driver validates, so a layout mismatch
// fails loudly with a status code rather than corrupting anything.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

const (
	nvapiIDInitialize           = 0x0150E828
	nvapiIDUnload               = 0xD22BDD7E
	nvapiIDDRSCreateSession     = 0x0694D52E
	nvapiIDDRSDestroySession    = 0xDAD9CFF8
	nvapiIDDRSLoadSettings      = 0x375DBD6B
	nvapiIDDRSSaveSettings      = 0xFCBC7E14
	nvapiIDDRSCreateProfile     = 0xCC176068
	nvapiIDDRSDeleteProfile     = 0x17093206
	nvapiIDDRSFindProfileByName = 0x7E4A9A0B
	nvapiIDDRSCreateApplication = 0x4347A9DE
	nvapiIDDRSFindAppByName     = 0xEEE566B2
	nvapiIDDRSGetSetting        = 0x73BF8338
	nvapiIDDRSSetSetting        = 0x577DD202

	// OGL_THREAD_CONTROL is the "Threaded optimization" switch; 2 = off.
	nvOGLThreadControlID      = 0x20C1221E
	nvOGLThreadControlDisable = 2

	nvDwordType = 0 // NVDRS_DWORD_TYPE
)

// errNoNVAPI reports a system without an NVIDIA driver — not an error in any
// meaningful sense, just "nothing to do here".
var errNoNVAPI = errors.New("nvapi64.dll not available")

// nvUnicode is NvAPI_UnicodeString: a fixed 2048-element UTF-16 buffer.
type nvUnicode [2048]uint16

func nvStr(s string) (out nvUnicode) {
	u := utf16.Encode([]rune(s))
	if len(u) > len(out)-1 {
		u = u[:len(out)-1]
	}
	copy(out[:], u)
	return out
}

// nvdrsProfile is NVDRS_PROFILE_V1 (sizeof 4116, version 0x11014).
type nvdrsProfile struct {
	version       uint32
	profileName   nvUnicode
	gpuSupport    uint32
	isPredefined  uint32
	numOfApps     uint32
	numOfSettings uint32
}

// nvdrsApplication is NVDRS_APPLICATION_V1 (sizeof 12296, version 0x13008).
type nvdrsApplication struct {
	version          uint32
	isPredefined     uint32
	appName          nvUnicode
	userFriendlyName nvUnicode
	launcher         nvUnicode
}

// nvdrsSettingValue is NVDRS_SETTING_UNION: 4100 bytes, a DWORD value lives in
// the first four.
type nvdrsSettingValue [4100]byte

func (v *nvdrsSettingValue) setU32(x uint32) {
	v[0] = byte(x)
	v[1] = byte(x >> 8)
	v[2] = byte(x >> 16)
	v[3] = byte(x >> 24)
}

func (v *nvdrsSettingValue) u32() uint32 {
	return uint32(v[0]) | uint32(v[1])<<8 | uint32(v[2])<<16 | uint32(v[3])<<24
}

// nvdrsSetting is NVDRS_SETTING_V1 (sizeof 12320, version 0x13020).
type nvdrsSetting struct {
	version             uint32
	settingName         nvUnicode
	settingID           uint32
	settingType         uint32
	settingLocation     uint32
	isCurrentPredefined uint32
	isPredefinedValid   uint32
	predefined          nvdrsSettingValue
	current             nvdrsSettingValue
}

func nvVersion(size uintptr) uint32 { return uint32(size) | 1<<16 }

// nvapiSession wraps one initialised NVAPI DRS session.
type nvapiSession struct {
	fns    map[uint32]uintptr
	handle uintptr
}

func (s *nvapiSession) call(id uint32, args ...uintptr) int32 {
	fn := s.fns[id]
	if fn == 0 {
		return -1 // NVAPI_ERROR — resolved at open, so this cannot happen
	}
	r, _, _ := syscall.SyscallN(fn, args...)
	return int32(uint32(r))
}

// nvapiOpen loads nvapi64.dll, resolves every function this file uses, and
// opens a DRS session with the current driver settings loaded.
func nvapiOpen() (*nvapiSession, error) {
	dll := syscall.NewLazyDLL("nvapi64.dll")
	if err := dll.Load(); err != nil {
		return nil, errNoNVAPI
	}
	qi := dll.NewProc("nvapi_QueryInterface")
	if err := qi.Find(); err != nil {
		return nil, errNoNVAPI
	}

	s := &nvapiSession{fns: make(map[uint32]uintptr)}
	for _, id := range []uint32{
		nvapiIDInitialize, nvapiIDUnload,
		nvapiIDDRSCreateSession, nvapiIDDRSDestroySession,
		nvapiIDDRSLoadSettings, nvapiIDDRSSaveSettings,
		nvapiIDDRSCreateProfile, nvapiIDDRSDeleteProfile, nvapiIDDRSFindProfileByName,
		nvapiIDDRSCreateApplication, nvapiIDDRSFindAppByName,
		nvapiIDDRSGetSetting, nvapiIDDRSSetSetting,
	} {
		fn, _, _ := qi.Call(uintptr(id))
		if fn == 0 {
			return nil, fmt.Errorf("nvapi function %#x not exported by this driver", id)
		}
		s.fns[id] = fn
	}

	if status := s.call(nvapiIDInitialize); status != 0 {
		return nil, fmt.Errorf("NvAPI_Initialize: status %d", status)
	}
	if status := s.call(nvapiIDDRSCreateSession, uintptr(unsafe.Pointer(&s.handle))); status != 0 {
		s.call(nvapiIDUnload)
		return nil, fmt.Errorf("NvAPI_DRS_CreateSession: status %d", status)
	}
	if status := s.call(nvapiIDDRSLoadSettings, s.handle); status != 0 {
		s.close()
		return nil, fmt.Errorf("NvAPI_DRS_LoadSettings: status %d", status)
	}
	return s, nil
}

func (s *nvapiSession) close() {
	if s.handle != 0 {
		s.call(nvapiIDDRSDestroySession, s.handle)
		s.handle = 0
	}
	s.call(nvapiIDUnload)
}

// profileForApp returns the DRS profile that owns appExe, creating profile
// profileName (and registering the application in it) when no profile has it.
func (s *nvapiSession) profileForApp(appExe, profileName string) (uintptr, error) {
	appName := nvStr(appExe)

	var app nvdrsApplication
	app.version = nvVersion(unsafe.Sizeof(app))
	var hProfile uintptr
	status := s.call(nvapiIDDRSFindAppByName, s.handle,
		uintptr(unsafe.Pointer(&appName)),
		uintptr(unsafe.Pointer(&hProfile)),
		uintptr(unsafe.Pointer(&app)))
	if status == 0 {
		return hProfile, nil
	}

	// Not registered anywhere: reuse our named profile if a previous run made
	// it, otherwise create it, then attach the executable.
	profName := nvStr(profileName)
	status = s.call(nvapiIDDRSFindProfileByName, s.handle,
		uintptr(unsafe.Pointer(&profName)),
		uintptr(unsafe.Pointer(&hProfile)))
	if status != 0 {
		var prof nvdrsProfile
		prof.version = nvVersion(unsafe.Sizeof(prof))
		prof.profileName = profName
		status = s.call(nvapiIDDRSCreateProfile, s.handle,
			uintptr(unsafe.Pointer(&prof)),
			uintptr(unsafe.Pointer(&hProfile)))
		if status != 0 {
			return 0, fmt.Errorf("NvAPI_DRS_CreateProfile: status %d", status)
		}
	}

	app = nvdrsApplication{}
	app.version = nvVersion(unsafe.Sizeof(app))
	app.appName = appName
	app.userFriendlyName = profName
	if status = s.call(nvapiIDDRSCreateApplication, s.handle, hProfile,
		uintptr(unsafe.Pointer(&app))); status != 0 {
		return 0, fmt.Errorf("NvAPI_DRS_CreateApplication: status %d", status)
	}
	return hProfile, nil
}

// threadControl reads the profile's current OGL_THREAD_CONTROL value; ok is
// false when the profile does not carry the setting.
func (s *nvapiSession) threadControl(hProfile uintptr) (value uint32, ok bool) {
	var setting nvdrsSetting
	setting.version = nvVersion(unsafe.Sizeof(setting))
	status := s.call(nvapiIDDRSGetSetting, s.handle, hProfile,
		uintptr(nvOGLThreadControlID),
		uintptr(unsafe.Pointer(&setting)))
	if status != 0 {
		return 0, false
	}
	return setting.current.u32(), true
}

// setThreadControlOff writes OGL_THREAD_CONTROL = disable and persists it.
func (s *nvapiSession) setThreadControlOff(hProfile uintptr) error {
	var setting nvdrsSetting
	setting.version = nvVersion(unsafe.Sizeof(setting))
	setting.settingID = nvOGLThreadControlID
	setting.settingType = nvDwordType
	setting.current.setU32(nvOGLThreadControlDisable)
	if status := s.call(nvapiIDDRSSetSetting, s.handle, hProfile,
		uintptr(unsafe.Pointer(&setting))); status != 0 {
		return fmt.Errorf("NvAPI_DRS_SetSetting: status %d", status)
	}
	if status := s.call(nvapiIDDRSSaveSettings, s.handle); status != 0 {
		return fmt.Errorf("NvAPI_DRS_SaveSettings: status %d", status)
	}
	return nil
}

// deleteProfileByName removes a named profile — used only by the self-test to
// clean up after itself.
func (s *nvapiSession) deleteProfileByName(profileName string) error {
	profName := nvStr(profileName)
	var hProfile uintptr
	if status := s.call(nvapiIDDRSFindProfileByName, s.handle,
		uintptr(unsafe.Pointer(&profName)),
		uintptr(unsafe.Pointer(&hProfile))); status != 0 {
		return fmt.Errorf("NvAPI_DRS_FindProfileByName: status %d", status)
	}
	if status := s.call(nvapiIDDRSDeleteProfile, s.handle, hProfile); status != 0 {
		return fmt.Errorf("NvAPI_DRS_DeleteProfile: status %d", status)
	}
	if status := s.call(nvapiIDDRSSaveSettings, s.handle); status != 0 {
		return fmt.Errorf("NvAPI_DRS_SaveSettings: status %d", status)
	}
	return nil
}

// nvapiEnsureThreadControlOff makes sure the driver profile for appExe has
// Threaded Optimization off, creating the profile if needed. It reports whether
// anything was written — false means the profile was already configured (or
// there is no NVIDIA driver, distinguished by errNoNVAPI).
func nvapiEnsureThreadControlOff(appExe, profileName string) (changed bool, err error) {
	s, err := nvapiOpen()
	if err != nil {
		return false, err
	}
	defer s.close()

	hProfile, err := s.profileForApp(appExe, profileName)
	if err != nil {
		return false, err
	}
	if v, ok := s.threadControl(hProfile); ok && v == nvOGLThreadControlDisable {
		return false, nil
	}
	if err := s.setThreadControlOff(hProfile); err != nil {
		return false, err
	}
	return true, nil
}

// disableNvidiaThreadedOptimization applies the driver-profile guard for the
// running executable. Called once at startup, before any GL context exists so
// a fresh write is picked up by this launch where the driver allows; at latest
// it applies from the next launch. Failures are logged and never fatal — the
// app runs fine without it, minus the artefact-free guarantee.
func disableNvidiaThreadedOptimization() {
	exe, err := os.Executable()
	if err != nil {
		logWarn("nvidia: cannot determine executable path: %v", err)
		return
	}
	appExe := strings.ToLower(filepath.Base(exe))

	changed, err := nvapiEnsureThreadControlOff(appExe, "dicomqr")
	switch {
	case errors.Is(err, errNoNVAPI):
		// Not an NVIDIA system — nothing to guard against.
	case err != nil:
		logWarn("nvidia: could not disable Threaded Optimization for %s: %v — "+
			"if line artefacts appear around windows during cine playback, set it off manually: "+
			"NVIDIA Control Panel > Manage 3D Settings > Program Settings > %s > Threaded optimization = Off",
			appExe, err, appExe)
	case changed:
		logInfo("nvidia: Threaded Optimization disabled for %s (driver profile written)", appExe)
	default:
		logInfo("nvidia: Threaded Optimization already off for %s", appExe)
	}
}
