package service

import (
	"errors"
	"fmt"
	"runtime"
	"strings"
	"unicode"
	"unsafe"

	tui "github.com/grindlemire/go-tui"
	"golang.org/x/sys/windows"
)

const clipboardUnicodeText = 13 // CF_UNICODETEXT

var (
	clipboardUser32  = windows.NewLazySystemDLL("user32.dll")
	clipboardKernel  = windows.NewLazySystemDLL("kernel32.dll")
	openClipboard    = clipboardUser32.NewProc("OpenClipboard")
	closeClipboard   = clipboardUser32.NewProc("CloseClipboard")
	getClipboardData = clipboardUser32.NewProc("GetClipboardData")
	setClipboardData = clipboardUser32.NewProc("SetClipboardData")
	emptyClipboard   = clipboardUser32.NewProc("EmptyClipboard")
	createWindow     = clipboardUser32.NewProc("CreateWindowExW")
	destroyWindow    = clipboardUser32.NewProc("DestroyWindow")
	globalAlloc      = clipboardKernel.NewProc("GlobalAlloc")
	globalFree       = clipboardKernel.NewProc("GlobalFree")
	globalLock       = clipboardKernel.NewProc("GlobalLock")
	globalUnlock     = clipboardKernel.NewProc("GlobalUnlock")
	globalSize       = clipboardKernel.NewProc("GlobalSize")
)

func (d *dashboard) clipboardKey(e tui.KeyEvent) bool {
	if d.confirm != nil || d.working != "" {
		return false
	}
	shortcut := e.Mod == 0 && e.Key == tui.KeyF2
	if f := d.importField(); f != nil && (shortcut || e.Key == tui.KeyRune && e.Mod == tui.ModCtrl && e.Rune == 'v') {
		text, err := readClipboardText()
		if err == nil {
			text = strings.TrimSpace(text)
			switch {
			case text == "":
				err = errors.New("clipboard has no text")
			case len(text) > maxFieldBytes-len(f.Value):
				err = errors.New("clipboard text is too long for this field")
			case strings.ContainsFunc(text, unicode.IsControl):
				err = errors.New("clipboard must contain a single line of text")
			default:
				f.Value += text
			}
		}
		d.clipboardStatus(err, "Pasted into VPN import")
		return true
	}
	if s := d.screen; s != nil && s.credentials[0] != "" && e.Mod == 0 {
		switch e.Key {
		case tui.KeyF2:
			d.clipboardStatus(writeClipboardText(s.credentials[0]), "SOCKS5 username copied")
			return true
		case tui.KeyF3:
			d.clipboardStatus(writeClipboardText(s.credentials[1]), "SOCKS5 password copied")
			return true
		}
	}
	return false
}

func (d *dashboard) clipboardHint() string {
	if d.confirm != nil || d.working != "" {
		return ""
	}
	if s := d.screen; s != nil && s.credentials[0] != "" {
		return "F2 Copy username  •  F3 Copy password"
	}
	if d.importField() != nil {
		return "Ctrl+V / F2 Paste VPN import"
	}
	return ""
}

func (d *dashboard) importField() *Field {
	if s := d.screen; s != nil && s.Save != nil && d.selection < len(s.Fields) {
		if f := &s.Fields[d.selection]; f.Secret && !f.Toggle {
			return f
		}
	}
	return nil
}

func (d *dashboard) clipboardStatus(err error, success string) {
	d.message = success
	if err != nil {
		d.message = "Error: " + err.Error()
	}
}

func readClipboardText() (string, error) {
	// Opening/closing the clipboard must happen on the same Windows thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if ok, _, err := openClipboard.Call(0); ok == 0 {
		return "", fmt.Errorf("open clipboard: %w", err)
	}
	defer closeClipboard.Call()
	h, _, err := getClipboardData.Call(clipboardUnicodeText)
	if h == 0 {
		return "", errors.New("clipboard has no text")
	}
	size, _, err := globalSize.Call(h)
	if size < 2 {
		return "", fmt.Errorf("read clipboard text size: %w", err)
	}
	p, _, err := globalLock.Call(h)
	if p == 0 {
		return "", fmt.Errorf("lock clipboard text: %w", err)
	}
	defer globalUnlock.Call(h)
	// Bound the view by the actual allocation and the editor's maximum input.
	// Read a terminator as well so oversized or malformed data is not truncated.
	buf := unsafe.Slice((*uint16)(win32Pointer(p)), int(min(size/2, maxFieldBytes+1)))
	for i, unit := range buf {
		if unit == 0 {
			return windows.UTF16ToString(buf[:i]), nil
		}
	}
	return "", errors.New("clipboard text is too long or not terminated")
}

func writeClipboardText(text string) error {
	data, err := windows.UTF16FromString(text)
	if err != nil {
		return fmt.Errorf("encode clipboard text: %w", err)
	}
	h, _, err := globalAlloc.Call(2, uintptr(len(data))*2) // GMEM_MOVEABLE
	if h == 0 {
		return fmt.Errorf("allocate clipboard text: %w", err)
	}
	defer func() {
		if h != 0 {
			globalFree.Call(h)
		}
	}()
	p, _, err := globalLock.Call(h)
	if p == 0 {
		return fmt.Errorf("lock clipboard text: %w", err)
	}
	copy(unsafe.Slice((*uint16)(win32Pointer(p)), len(data)), data)
	globalUnlock.Call(h)

	// A local message-only window owns the clipboard in either conhost or a
	// pseudoconsole. OpenClipboard(NULL) cannot set data after EmptyClipboard.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	class := [...]uint16{'S', 'T', 'A', 'T', 'I', 'C', 0}
	owner, _, err := createWindow.Call(0, uintptr(unsafe.Pointer(&class[0])), 0, 0, 0, 0, 0, 0, ^uintptr(2), 0, 0, 0) // HWND_MESSAGE (-3)
	runtime.KeepAlive(&class)
	if owner == 0 {
		return fmt.Errorf("create clipboard owner: %w", err)
	}
	defer destroyWindow.Call(owner)
	if ok, _, err := openClipboard.Call(owner); ok == 0 {
		return fmt.Errorf("open clipboard: %w", err)
	}
	defer closeClipboard.Call()
	if ok, _, err := emptyClipboard.Call(); ok == 0 {
		return fmt.Errorf("empty clipboard: %w", err)
	}
	if ok, _, err := setClipboardData.Call(clipboardUnicodeText, h); ok == 0 {
		return fmt.Errorf("copy clipboard text: %w", err)
	}
	h = 0 // Windows now owns this allocation.
	return nil
}

// Win32 returns an address to non-Go memory in a syscall result word.
func win32Pointer(address uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&address))
}
