package service

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	tui "github.com/grindlemire/go-tui"
)

func TestWindowsPasteImportFromClipboard(t *testing.T) {
	requireClipboardFixture(t)
	want := "btawg://" + strings.Repeat("abcXYZ09-_", 460)
	setFixtureClipboard(t, want+"\r\n")
	for _, key := range []tui.KeyEvent{
		{Key: tui.KeyRune, Rune: 'v', Mod: tui.ModCtrl},
		{Key: tui.KeyF2},
	} {
		d := dashboard{app: new(tui.App), screen: &Screen{
			Fields: []Field{{Label: "VPN import", Secret: true}},
			Save:   func([]Field) (*Screen, error) { return nil, nil },
		}}
		d.key(key)
		if got := d.screen.Fields[0].Value; got != want {
			t.Errorf("%v: paste length = %d, want %d (status %q)", key.Key, len(got), len(want), d.message)
		}
	}
}

func TestWindowsCopySOCKSCredentialsSeparately(t *testing.T) {
	requireClipboardFixture(t)
	d := dashboard{app: new(tui.App), screen: SOCKSCredentialsScreen("SOCKS5 credentials", "proxy-user", "proxy-Pass_09!")}
	for _, tc := range []struct {
		key  tui.Key
		want string
	}{
		{tui.KeyF2, "proxy-user"},
		{tui.KeyF3, "proxy-Pass_09!"},
	} {
		setFixtureClipboard(t, "not the credential")
		d.key(tui.KeyEvent{Key: tc.key})
		cmd := exec.Command("powershell.exe", "-NoProfile", "-Command", "[Console]::Write((Get-Clipboard -Raw))")
		out, err := cmd.CombinedOutput()
		if err != nil || string(out) != tc.want {
			t.Errorf("%v: copied %q, want exact value %q: %v (status %q)", tc.key, out, tc.want, err, d.message)
		}
	}
}

func requireClipboardFixture(t *testing.T) {
	t.Helper()
	if os.Getenv("BTAWG_WINDOWS_CLIPBOARD_TESTS") != "1" {
		t.Skip("requires a disposable interactive Windows desktop; changes its clipboard")
	}
}

func setFixtureClipboard(t *testing.T, text string) {
	t.Helper()
	cmd := exec.Command("powershell.exe", "-NoProfile", "-Command", "Set-Clipboard -Value ([Console]::In.ReadToEnd())")
	cmd.Stdin = strings.NewReader(text)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture could not set clipboard: %v: %s", err, out)
	}
}
