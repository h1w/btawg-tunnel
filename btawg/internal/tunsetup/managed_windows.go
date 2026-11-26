package tunsetup

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/amnezia-vpn/amneziawg-go/v3/tun"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.zx2c4.com/wintun"
)

const windowsAdapterRemovalTimeout = 10 * time.Second

var windowsAdapterPollInterval = 50 * time.Millisecond

// WindowsAdapterIdentity identifies an adapter instance, not its mutable alias.
type WindowsAdapterIdentity struct {
	LUID  uint64
	GUID  windows.GUID
	Index uint32
	Alias string
}

func adapterIdentity(row windows.MibIfRow2) WindowsAdapterIdentity {
	return WindowsAdapterIdentity{
		LUID:  row.InterfaceLuid,
		GUID:  row.InterfaceGuid,
		Index: row.InterfaceIndex,
		Alias: windows.UTF16ToString(row.Alias[:]),
	}
}

// A stale IP Helper row can outlive the Wintun device instance. Require the
// corresponding SWD\Wintun device key before treating a GUID as a live adapter.
func wintunDeviceRegistered(guid windows.GUID) (bool, error) {
	path := `SYSTEM\CurrentControlSet\Enum\SWD\Wintun\` + strings.ToUpper(guid.String())
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_ = key.Close()
	return true, nil
}

// FindWindowsAdapterIdentity returns a registered Wintun device by alias.
// Its absence does not authorize modifying any other adapter with that alias.
func FindWindowsAdapterIdentity(alias string) (WindowsAdapterIdentity, bool, error) {
	row, err := findWindowsAdapter(alias)
	if err != nil || row.InterfaceLuid == 0 {
		return WindowsAdapterIdentity{}, false, err
	}
	description := strings.ToLower(windows.UTF16ToString(row.Description[:]))
	if !strings.Contains(description, "wintun") && !strings.Contains(description, "btawg") {
		return WindowsAdapterIdentity{}, false, nil
	}
	registered, err := wintunDeviceRegistered(row.InterfaceGuid)
	if err != nil || !registered {
		return WindowsAdapterIdentity{}, false, err
	}
	return adapterIdentity(row), true, nil
}

// AdapterIdentityPresent checks the adapter instance by GUID/LUID and live PnP
// registration. The alias may be foreign or stale and is not consulted.
func AdapterIdentityPresent(id WindowsAdapterIdentity) (bool, error) {
	if id.LUID == 0 {
		return false, nil
	}
	registered, err := wintunDeviceRegistered(id.GUID)
	if err != nil || !registered {
		return false, err
	}
	row, err := findWindowsAdapterByGUID(id.GUID)
	if err != nil || row.InterfaceLuid == 0 {
		return false, err
	}
	return row.InterfaceLuid == id.LUID, nil
}

func waitForWindowsAdapterRemoval(ctx context.Context, id WindowsAdapterIdentity, present func(WindowsAdapterIdentity) (bool, error), interval time.Duration) error {
	if id.LUID == 0 {
		return nil
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		found, err := present(id)
		if err != nil || !found {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("adapter %s (LUID %#x) still present: %w", id.GUID, id.LUID, ctx.Err())
		case <-ticker.C:
		}
	}
}

// WaitForWindowsAdapterRemoval waits until the identified adapter instance is
// gone; Wintun deletion continues after the closing handle returns.
func WaitForWindowsAdapterRemoval(ctx context.Context, id WindowsAdapterIdentity) error {
	ctx, cancel := context.WithTimeout(ctx, windowsAdapterRemovalTimeout)
	defer cancel()
	return waitForWindowsAdapterRemoval(ctx, id, AdapterIdentityPresent, windowsAdapterPollInterval)
}

// WintunCloseAdapter has no status result. The Go binding can surface a stale
// last-error, so adapter identity removal is the authority on close success.
func closeOwnedWindowsAdapter(adapter *wintun.Adapter, id WindowsAdapterIdentity, causes ...error) error {
	closeErr := adapter.Close()
	ctx, cancel := context.WithTimeout(context.Background(), windowsAdapterRemovalTimeout)
	defer cancel()
	waitErr := waitForWindowsAdapterRemoval(ctx, id, AdapterIdentityPresent, windowsAdapterPollInterval)
	if waitErr != nil {
		return errors.Join(append(causes, closeErr, waitErr)...)
	}
	return errors.Join(causes...)
}

func waitForAliasReleased(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		row, err := findWindowsAdapter(name)
		if err != nil || row.InterfaceLuid == 0 {
			return err
		}
		if !strings.EqualFold(windows.UTF16ToString(row.Description[:]), "btawg Tunnel") {
			return fmt.Errorf("interface %q is already in use", name)
		}
		live, err := wintunDeviceRegistered(row.InterfaceGuid)
		if err != nil || !live {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("interface %q is already in use", name)
		case <-ticker.C:
		}
	}
}

// OpenClientTUN creates a Wintun adapter with the assigned VPN address and
// overlay routes. No foreign adapter or default route is modified.
func OpenClientTUN(name string, addr netip.Prefix, routes []string, mtu int) (tun.Device, error) {
	if name == "" || !addr.IsValid() || !addr.Addr().Is4() || addr.Bits() == 0 || mtu <= 0 || mtu > 65535 {
		return nil, fmt.Errorf("invalid Windows tunnel settings for %q (%s, MTU %d)", name, addr, mtu)
	}
	prefixes := make([]netip.Prefix, 0, len(routes))
	for _, raw := range routes {
		p, err := netip.ParsePrefix(raw)
		if err != nil || !p.Addr().Is4() || p.Bits() == 0 {
			return nil, fmt.Errorf("invalid overlay route %q", raw)
		}
		p = p.Masked()
		if p != addr.Masked() && !slices.Contains(prefixes, p) {
			prefixes = append(prefixes, p)
		}
	}
	row, err := findWindowsAdapter(name)
	if err != nil {
		return nil, fmt.Errorf("look up adapter %q: %w", name, err)
	}
	if row.InterfaceLuid != 0 {
		if err := waitForAliasReleased(name); err != nil {
			return nil, err
		}
	}
	if err := rejectWindowsRouteOverlap(0, addr, prefixes); err != nil {
		return nil, err
	}

	// WintunCreateAdapter can rename an adapter already using the requested
	// name. Create under a unique temporary alias, then rename only our adapter
	// through NetAdapter, which rejects an existing destination alias.
	id, err := windows.GenerateGUID()
	if err != nil {
		return nil, err
	}
	tmpName := "btawg-" + id.String()[1:37]
	adapter, err := wintun.CreateAdapter(tmpName, "btawg", &id)
	if err != nil {
		return nil, fmt.Errorf("create Wintun adapter: %w", err)
	}
	luid := adapter.LUID()
	identity := WindowsAdapterIdentity{LUID: luid, GUID: id, Alias: name}
	closeOnFailure := func(causes ...error) error {
		return closeOwnedWindowsAdapter(adapter, identity, causes...)
	}
	registerUnderlayAdapter(luid, true)
	ok := false
	defer func() {
		if !ok {
			registerUnderlayAdapter(luid, false)
		}
	}()
	if err := renameWindowsAdapter(luid, name); err != nil {
		return nil, closeOnFailure(err)
	}
	if err := rejectWindowsRouteOverlap(luid, addr, prefixes); err != nil {
		return nil, closeOnFailure(err)
	}
	if err := configureWindowsTUN(luid, addr, prefixes, mtu); err != nil {
		return nil, closeOnFailure(err)
	}
	session, err := adapter.StartSession(8 << 20)
	if err != nil {
		return nil, closeOnFailure(err)
	}
	stop, err := windows.CreateEvent(nil, 1, 0, nil)
	if err != nil {
		session.End()
		return nil, closeOnFailure(err)
	}
	events := make(chan tun.Event, 1)
	events <- tun.EventUp
	ok = true
	return &windowsTUN{adapter: adapter, session: session, name: name, mtu: mtu, stop: stop, events: events, identity: identity}, nil
}

func renameWindowsAdapter(luid uint64, name string) error {
	row := windows.MibIfRow2{InterfaceLuid: luid}
	if err := windows.GetIfEntry2Ex(windows.MibIfTableNormalWithoutStatistics, &row); err != nil {
		return fmt.Errorf("look up Wintun adapter: %w", err)
	}
	dir, err := windows.GetSystemDirectory()
	if err != nil {
		return err
	}
	const script = `$ErrorActionPreference = 'Stop'
$adapter = Get-NetAdapter -IncludeHidden | Where-Object { [guid]$_.InterfaceGuid -eq [guid]$env:BTAWG_ADAPTER_GUID }
if (@($adapter).Count -ne 1) { throw 'Could not uniquely identify the new Wintun adapter' }
Rename-NetAdapter -InputObject $adapter -NewName $env:BTAWG_ADAPTER_NAME -Confirm:$false`
	cmd := exec.Command(filepath.Join(dir, "WindowsPowerShell", "v1.0", "powershell.exe"), "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Env = append(os.Environ(), "BTAWG_ADAPTER_GUID="+row.InterfaceGuid.String(), "BTAWG_ADAPTER_NAME="+name)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("rename Wintun adapter to %q: %w: %s", name, err, output)
	}
	return nil
}

type windowsTUN struct {
	adapter  *wintun.Adapter
	session  wintun.Session
	identity WindowsAdapterIdentity
	name     string
	mtu      int
	stop     windows.Handle
	events   chan tun.Event
	mu       sync.Mutex // Serializes write, close and reader admission.
	reading  sync.WaitGroup
	closed   bool
}

func (t *windowsTUN) Name() (string, error)    { return t.name, nil }
func (t *windowsTUN) MTU() (int, error)        { return t.mtu, nil }
func (t *windowsTUN) BatchSize() int           { return 1 }
func (t *windowsTUN) File() *os.File           { return nil }
func (t *windowsTUN) Events() <-chan tun.Event { return t.events }

func (t *windowsTUN) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return 0, os.ErrClosed
	}
	t.reading.Add(1)
	t.mu.Unlock()
	defer t.reading.Done()
	for {
		packet, err := t.session.ReceivePacket()
		switch err {
		case nil:
			if len(packet) > len(bufs[0])-offset {
				t.session.ReleaseReceivePacket(packet)
				return 0, io.ErrShortBuffer
			}
			sizes[0] = copy(bufs[0][offset:], packet)
			t.session.ReleaseReceivePacket(packet)
			return 1, nil
		case windows.ERROR_NO_MORE_ITEMS:
			events := [...]windows.Handle{t.stop, t.session.ReadWaitEvent()}
			which, err := windows.WaitForMultipleObjects(events[:], false, windows.INFINITE)
			if err != nil {
				return 0, err
			}
			if which == windows.WAIT_OBJECT_0 {
				return 0, os.ErrClosed
			}
		case windows.ERROR_HANDLE_EOF:
			return 0, os.ErrClosed
		default:
			return 0, err
		}
	}
}

func (t *windowsTUN) Write(bufs [][]byte, offset int) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return 0, os.ErrClosed
	}
	for i, b := range bufs {
		packet, err := t.session.AllocateSendPacket(len(b) - offset)
		if err == windows.ERROR_BUFFER_OVERFLOW {
			continue
		}
		if err != nil {
			return i, err
		}
		copy(packet, b[offset:])
		t.session.SendPacket(packet)
	}
	return len(bufs), nil
}

func (t *windowsTUN) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil
	}
	if err := windows.SetEvent(t.stop); err != nil {
		return err
	}
	t.closed = true
	t.reading.Wait()
	t.session.End()
	close(t.events)
	luid := t.adapter.LUID()
	defer registerUnderlayAdapter(luid, false)
	return closeOwnedWindowsAdapter(t.adapter, t.identity, windows.CloseHandle(t.stop))
}

func (t *windowsTUN) interfaceIndex() (uint32, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return 0, net.ErrClosed
	}
	row := windows.MibIfRow2{InterfaceLuid: t.adapter.LUID()}
	if err := windows.GetIfEntry2Ex(windows.MibIfTableNormalWithoutStatistics, &row); err != nil {
		return 0, fmt.Errorf("look up owned tunnel interface: %w", err)
	}
	if row.InterfaceLuid != t.adapter.LUID() || row.InterfaceIndex == 0 {
		return 0, fmt.Errorf("owned tunnel interface is unavailable")
	}
	return row.InterfaceIndex, nil
}

// BindOwnedDevice binds a listener to the adapter instance, not its mutable alias.
func BindOwnedDevice(dev interface{}, _ string) func(string, string, syscall.RawConn) error {
	return func(_, _ string, conn syscall.RawConn) error {
		t, ok := dev.(*windowsTUN)
		if !ok {
			return errors.New("listener has no owned Windows tunnel")
		}
		index, err := t.interfaceIndex()
		if err != nil {
			return err
		}
		return bindWindowsInterface(conn, index)
	}
}

func bindWindowsInterface(conn syscall.RawConn, index uint32) error {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], index)
	var sockErr error
	if err := conn.Control(func(fd uintptr) {
		sockErr = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, 31, int(binary.NativeEndian.Uint32(encoded[:])))
	}); err != nil {
		return err
	}
	return sockErr
}

// BindDevice selects the IPv4 output interface of an overlay listener. Callers
// also bind the listener's local address to the VPN address.
func BindDevice(name string) func(string, string, syscall.RawConn) error {
	return func(_, _ string, conn syscall.RawConn) error {
		row, err := findWindowsAdapter(name)
		if err != nil {
			return err
		}
		if row.InterfaceIndex == 0 {
			return fmt.Errorf("interface %q not found", name)
		}
		const ipUnicastIF = 31
		var index [4]byte
		binary.BigEndian.PutUint32(index[:], row.InterfaceIndex)
		if err := conn.Control(func(fd uintptr) {
			err = windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, ipUnicastIF, int(binary.NativeEndian.Uint32(index[:])))
		}); err != nil {
			return err
		}
		return err
	}
}
