package service

import (
	"context"
	"errors"
	"slices"
	"testing"

	tui "github.com/grindlemire/go-tui"
)

func TestServiceSettingsSaveRestartsOnlyRunningService(t *testing.T) {
	for _, state := range []string{"active", "inactive", "failed"} {
		t.Run(state, func(t *testing.T) {
			var events []string
			old := systemctlRunner
			t.Cleanup(func() { systemctlRunner = old })
			systemctlRunner = func(_ context.Context, args ...string) (string, error) {
				if args[0] == "show" {
					return "LoadState=loaded\nActiveState=" + state + "\n", nil
				}
				events = append(events, args[0])
				return "", nil
			}
			invalid := errors.New("invalid configuration")
			d := dashboard{root: true, current: snapshot{state: state}, menu: Menu{Unit: Client, Setup: func() (*Screen, error) {
				return &Screen{Fields: []Field{{Value: "before"}}, Save: func(fields []Field) (*Screen, error) {
					if fields[0].Value == "invalid" {
						return nil, invalid
					}
					events = append(events, "save:"+fields[0].Value)
					return nil, nil
				}}, nil
			}}}
			items := d.actions()
			i := slices.IndexFunc(items, func(a Action) bool { return a.Label == "Service settings" })
			if i < 0 {
				t.Fatal("settings unavailable")
			}
			s, err := items[i].Run()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Save(s.Fields); err != nil || len(events) != 0 {
				t.Fatalf("unchanged settings: %v, %v", events, err)
			}
			s.Fields[0].Value = "invalid"
			if _, err := s.Save(s.Fields); !errors.Is(err, invalid) || len(events) != 0 {
				t.Fatalf("invalid settings: %v, %v", events, err)
			}
			s.Fields[0].Value = "after"
			if _, err := s.Save(s.Fields); err != nil {
				t.Fatal(err)
			}
			want := []string{"save:after"}
			if state == "active" {
				want = append(want, "try-restart")
			}
			if !slices.Equal(events, want) {
				t.Fatalf("operations = %v, want %v", events, want)
			}
		})
	}
}

func TestReorderAcceptsUnshiftedPlusKey(t *testing.T) {
	d := dashboard{app: new(tui.App), screen: &Screen{Reorder: true, Fields: []Field{{Label: "a", Toggle: true}, {Label: "b", Toggle: true}}, Save: func([]Field) (*Screen, error) { return nil, nil }}}
	d.key(tui.KeyEvent{Key: tui.KeyRune, Rune: '='})
	if d.screen.Fields[0].Label != "b" || d.selection != 1 {
		t.Fatal("unshifted plus key did not move the row down")
	}
	d.key(tui.KeyEvent{Key: tui.KeyRune, Rune: '-'})
	if d.screen.Fields[0].Label != "a" || d.selection != 0 {
		t.Fatal("minus did not move the row up")
	}
}

func TestTextFieldLeavesPasteMouseGestureToTerminal(t *testing.T) {
	term := tui.NewMockTerminal(80, 24)
	term.EnableMouse()
	d := dashboard{screen: &Screen{Fields: []Field{{Secret: true}, {Toggle: true}}, Save: func([]Field) (*Screen, error) { return nil, nil }}}
	d.syncInputMode(term)
	if term.IsMouseEnabled() {
		t.Fatal("mouse reporting still intercepts terminal paste in import field")
	}
	d.selection = 1
	d.syncInputMode(term)
	if !term.IsMouseEnabled() {
		t.Fatal("mouse reporting not restored after leaving import field")
	}
}

func TestDocumentLeavesTextSelectionToTerminal(t *testing.T) {
	term := tui.NewMockTerminal(80, 24)
	term.EnableMouse()
	d := dashboard{app: new(tui.App)}
	d.open(&Screen{Text: "Username: operator\nPassword: test-only", Dismiss: func() error { return nil }})
	d.syncInputMode(term)
	if term.IsMouseEnabled() {
		t.Fatal("mouse reporting prevents selecting the displayed credentials")
	}
	d.key(tui.KeyEvent{Key: tui.KeyEscape})
	d.syncInputMode(term)
	if !term.IsMouseEnabled() {
		t.Fatal("returning from the document did not restore menu clicks")
	}
}

func TestSelectionShortcutKeepsTerminalInControlUntilToggledOff(t *testing.T) {
	term := tui.NewMockTerminal(80, 24)
	term.EnableMouse()
	d := dashboard{app: new(tui.App)}
	shortcut := tui.KeyEvent{Key: tui.KeyRune, Rune: 't', Mod: tui.ModCtrl}
	d.key(shortcut)
	d.syncInputMode(term)
	if term.IsMouseEnabled() {
		t.Fatal("Ctrl+T did not release mouse capture")
	}
	d.open(&Screen{Choices: []Action{{Label: "Next"}}})
	d.syncInputMode(term)
	if term.IsMouseEnabled() {
		t.Fatal("opening another menu recaptured the mouse")
	}
	d.key(shortcut)
	d.syncInputMode(term)
	if !term.IsMouseEnabled() {
		t.Fatal("second Ctrl+T did not restore menu clicks")
	}
}

func TestGitHubUpdateKeepsDashboardNavigationResponsive(t *testing.T) {
	d := dashboard{
		app: new(tui.App), menu: Menu{Unit: Client}, root: true,
		current: snapshot{state: "active"}, busy: true, working: updateActionLabel,
	}
	d.key(tui.KeyEvent{Key: tui.KeyDown})
	if got := d.actions()[d.selection].Label; got != "Restart service" {
		t.Fatalf("Down selects %q during the GitHub update, want Restart service", got)
	}
}
