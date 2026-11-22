package service

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	tui "github.com/grindlemire/go-tui"
)

// Menu is the operator interface for one managed service. Role-specific actions
// share the same renderer, navigation, and operation queue as systemd actions.
type Menu struct {
	Unit                 Unit
	Subtitle             string
	Setup                func() (*Screen, error)
	Install              func() (*Screen, error)
	Actions              func(state string) ([]Action, error)
	SetupRestartsService bool
}

type Action struct {
	Label   string
	Confirm string
	Run     func() (*Screen, error)
}

// Screen is a choice list, a document (optionally acknowledged), or a setup
// form. Secret fields are never rendered, including when pasted into the terminal.
type Screen struct {
	Title   string
	Text    string
	Fields  []Field
	Choices []Action // an alternative to a form or document
	Submit  string
	Save    func([]Field) (*Screen, error)
	Dismiss func() error
	Reorder bool // -/+ move the highlighted form field up/down

	credentials [2]string // raw SOCKS5 values, independent of the display text
}

// SOCKSCredentialsScreen shows the values separately for display and copying.
func SOCKSCredentialsScreen(title, username, password string) *Screen {
	return &Screen{
		Title: title, Text: "Username:\n" + username + "\nPassword:\n" + password,
		credentials: [2]string{username, password},
	}
}

type Field struct {
	Label   string
	Value   string
	Secret  bool
	Toggle  bool
	Checked bool
}

const updateActionLabel = "Update from GitHub"
const maxFieldBytes = 16384

func TUI(menu Menu) error {
	restore, err := prepareDashboardConsole()
	if err != nil {
		return err
	}
	defer restore()
	d := &dashboard{menu: menu, root: Privileged(), results: make(chan update, 1)}
	if menu.Unit == Client && d.root {
		d.message = platformUpdateNotice()
	}
	var notices chan string
	if menu.Unit == Client && menu.Unit.releaseAsset(runtime.GOOS, runtime.GOARCH) != "" {
		notices = make(chan string, 1)
		d.releaseNotices = notices
	}
	app, err := tui.NewApp(tui.WithRootComponent(d), tui.WithMouse(), tui.WithOnResume(func() {
		d.mouseReleased = false // go-tui re-enables reporting when it resumes.
	}))
	if err != nil {
		return err
	}
	defer app.Close()
	defer func() {
		if d.cancelUpdate != nil {
			d.cancelUpdate()
		}
		if d.updateDone != nil {
			<-d.updateDone // let installation or rollback finish before process exit
		}
	}()
	if notices != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		go func() {
			client := directUpdateClient()
			defer client.CloseIdleConnections()
			notices <- menu.Unit.availableRelease(ctx, client)
		}()
	}
	d.refresh()
	return app.Run()
}

type snapshot struct {
	state   string
	enabled bool
	actions []Action
	err     error
}

type update struct {
	snapshot
	screen    *Screen
	err       error
	operation string
}

type dashboard struct {
	menu    Menu
	root    bool
	results chan update
	current snapshot
	busy    bool
	working string
	pending func() (*Screen, error)
	app     *tui.App

	releaseNotices <-chan string
	newRelease     string

	// Only the download is cancellable. The worker owns installation through rollback.
	cancelUpdate    context.CancelFunc
	updateDone      <-chan struct{}
	stoppingUpdate  bool
	quitAfterUpdate bool
	updateProgress  atomic.Value // string; published by the update worker

	screen        *Screen
	history       []*Screen
	confirm       *Action
	message       string
	selection     int
	offset        int
	hits          []hit
	selectText    bool // keep mouse capture off across screens
	mouseReleased bool // the terminal currently handles the mouse
}

type hit struct {
	element *tui.Element
	index   int
}

func (d *dashboard) status() snapshot {
	var s snapshot
	s.state, s.enabled, s.err = d.menu.Unit.State()
	if s.err == nil && d.root && d.menu.Actions != nil {
		s.actions, s.err = d.menu.Actions(s.state)
	}
	return s
}

func (d *dashboard) refresh() {
	if !d.busy && d.screen == nil && d.confirm == nil {
		d.run("", nil)
	}
}

func (d *dashboard) run(label string, fn func() (*Screen, error)) {
	if d.busy {
		if label != "" && d.working == "" {
			d.working, d.pending = label, fn
			d.message = ""
			d.app.MarkDirty()
		}
		return
	}
	d.busy, d.working = true, label
	if label != "" {
		d.message = ""
		d.app.MarkDirty()
	}
	var done chan struct{}
	if label == updateActionLabel {
		done = make(chan struct{})
		d.updateDone = done
	}
	current := d.current
	go func() {
		r := update{operation: label}
		if fn != nil {
			r.screen, r.err = fn()
		}
		if done != nil {
			close(done)
			r.snapshot = current // Do not delay cancellation on a service status probe.
		} else {
			r.snapshot = d.status()
		}
		d.results <- r
	}()
}

func (d *dashboard) Watchers() []tui.Watcher {
	watchers := []tui.Watcher{
		tui.Watch(d.results, d.receive),
		tui.OnTimer(2*time.Second, d.refresh),
	}
	if d.releaseNotices != nil {
		watchers = append(watchers, tui.Watch(d.releaseNotices, d.receiveReleaseNotice))
	}
	return watchers
}

func (d *dashboard) receiveReleaseNotice(version string) {
	if d.releaseNotices != nil {
		d.newRelease = version
		d.app.MarkDirty()
	}
}

func (d *dashboard) receive(u update) {
	oldState := d.current.state
	label := d.working
	d.current, d.busy, d.working = u.snapshot, false, ""
	if u.operation == updateActionLabel {
		d.cancelUpdate()
		d.cancelUpdate, d.updateDone, d.stoppingUpdate = nil, nil, false
		if d.quitAfterUpdate {
			d.app.Stop()
			return
		}
		d.quitAfterUpdate = false
	}
	if u.err == nil && u.operation == updateActionLabel {
		// Ignore any startup result that arrives after the manual check.
		d.releaseNotices, d.newRelease = nil, ""
	}
	if d.pending != nil {
		fn := d.pending
		d.pending = nil
		d.run(label, fn)
		return
	}
	switch {
	case u.operation == updateActionLabel && errors.Is(u.err, context.Canceled):
		d.message = "GitHub update cancelled"
	case u.err != nil:
		d.message = "Error: " + u.err.Error()
	case u.screen != nil:
		if d.screen != nil && len(d.screen.Choices) > 0 {
			d.history = append(d.history, d.screen)
		}
		d.open(u.screen)
	case u.operation != "":
		if d.screen != nil && (d.screen.Save != nil || len(d.screen.Choices) > 0) {
			d.open(nil)
			d.history = nil
		}
		d.message = u.operation + ": done"
	}
	if d.screen == nil && d.confirm == nil && oldState != d.current.state {
		d.selection, d.offset = 0, 0
	}
	d.app.MarkDirty()
}

func (d *dashboard) open(s *Screen) {
	d.screen = s
	d.selection, d.offset = 0, 0
}

func perform(fn func() error) func() (*Screen, error) {
	return func() (*Screen, error) { return nil, fn() }
}

func (d *dashboard) actions() []Action {
	u, s := d.menu.Unit, d.current
	var out []Action
	if d.root && s.state != "" {
		if s.state == NotInstalled {
			out = append(out, Action{Label: "Install service", Run: d.menu.Install})
		} else {
			if s.state == "active" || s.state == "activating" || s.state == "reloading" {
				out = append(out, Action{Label: "Stop service", Run: perform(func() error { return u.Control("stop") })})
			} else {
				out = append(out, Action{Label: "Start service", Run: perform(func() error { return u.Control("start") })})
			}
			out = append(out, Action{Label: "Restart service", Run: perform(func() error { return u.Control("restart") })})
			label, cmd := "Enable autostart", "enable"
			if s.enabled {
				label, cmd = "Disable autostart", "disable"
			}
			out = append(out, Action{Label: label, Run: perform(func() error { return u.Control(cmd) })})
		}
		if d.menu.Setup != nil {
			out = append(out, Action{Label: "Service settings", Run: d.settings})
		}
		out = append(out, s.actions...)
		if s.state != NotInstalled {
			out = append(out,
				Action{Label: updateActionLabel, Confirm: "Download the latest release directly? A running service will restart."},
				Action{Label: "Remove service", Confirm: "Stop and remove the service? Configuration, keys, and the installed launcher are kept.", Run: perform(u.Uninstall)},
			)
		}
	}
	return append(out, Action{Label: "View service logs", Run: u.logs}, Action{Label: "Quit"})
}

func (d *dashboard) settings() (*Screen, error) {
	s, err := d.menu.Setup()
	if err != nil || s == nil || s.Save == nil {
		return s, err
	}
	initial, save := slices.Clone(s.Fields), s.Save
	unit := d.menu.Unit
	s.Save = func(fields []Field) (*Screen, error) {
		if slices.Equal(initial, fields) {
			return nil, nil
		}
		next, err := save(fields)
		if err != nil || d.menu.SetupRestartsService {
			return next, err
		}
		state, _, err := unit.State()
		if err != nil {
			return next, fmt.Errorf("settings saved; check service state: %w", err)
		}
		if state == "active" || state == "activating" || state == "reloading" {
			if err := unit.Control("try-restart"); err != nil {
				return next, fmt.Errorf("settings saved; restart failed: %w", err)
			}
		}
		return next, nil
	}
	return s, nil
}

func (d *dashboard) syncInputMode(term tui.Terminal) {
	s := d.screen
	releaseMouse := d.selectText || s != nil && len(s.Choices) == 0 &&
		(s.Save == nil || d.selection < len(s.Fields) && !s.Fields[d.selection].Toggle)
	if releaseMouse == d.mouseReleased {
		return
	}
	d.mouseReleased = releaseMouse
	// Let the terminal select/copy documents and paste into focused text fields.
	if releaseMouse {
		term.DisableMouse()
	} else {
		term.EnableMouse()
	}
}

func (d *dashboard) activate() {
	if d.busy && d.working != "" {
		if d.working == updateActionLabel {
			items := d.actions()
			if d.selection < len(items) && items[d.selection].Label == "Quit" {
				d.interruptUpdate(true)
			}
		}
		return
	}
	if a := d.confirm; a != nil {
		d.confirm = nil
		d.selection = 0
		if d.offset == 1 {
			d.offset = 0
			if a.Label == updateActionLabel {
				ctx, cancel := context.WithCancel(context.Background())
				d.cancelUpdate = cancel
				d.updateProgress.Store("Waiting to check latest release…")
				app, unit := d.app, d.menu.Unit
				d.run(a.Label, func() (*Screen, error) {
					return unit.updateScreen(ctx, func(progress string) {
						d.updateProgress.Store(progress)
						app.MarkDirty()
					})
				})
			} else {
				d.run(a.Label, a.Run)
			}
		} else {
			d.offset = 0
		}
		return
	}
	if s := d.screen; s != nil {
		if len(s.Choices) > 0 {
			a := s.Choices[d.selection]
			d.run(a.Label, a.Run)
		} else if s.Save == nil {
			if s.Dismiss != nil {
				if err := s.Dismiss(); err != nil {
					d.message = "Error: " + err.Error()
					return
				}
			}
			d.back()
		} else if d.selection == len(s.Fields) {
			d.run(s.Submit, func() (*Screen, error) { return s.Save(s.Fields) })
		} else if f := &s.Fields[d.selection]; f.Toggle {
			f.Checked = !f.Checked
		} else {
			d.move(1)
		}
		return
	}
	items := d.actions()
	if d.selection >= len(items) {
		return
	}
	a := items[d.selection]
	if a.Confirm != "" {
		d.confirm = &a
		d.selection, d.offset = 0, 0
		d.message = ""
	} else if a.Run == nil {
		d.app.Stop()
	} else {
		d.run(a.Label, a.Run)
	}
}

func (d *dashboard) interruptUpdate(quit bool) {
	d.cancelUpdate()
	if d.updateDone == nil {
		// The update is still queued behind a status refresh.
		d.cancelUpdate, d.pending, d.working = nil, nil, ""
		d.message = "GitHub update cancelled"
		if quit {
			d.app.Stop()
		}
	} else {
		d.stoppingUpdate = true
		d.quitAfterUpdate = d.quitAfterUpdate || quit
	}
	d.app.MarkDirty()
}

func (d *dashboard) back() {
	if d.busy && d.working != "" {
		return
	}
	if d.confirm != nil {
		d.confirm = nil
		d.selection, d.offset = 0, 0
		return
	}
	if d.screen == nil {
		d.app.Stop()
		return
	}
	if n := len(d.history); n > 0 {
		d.open(d.history[n-1])
		d.history[n-1] = nil
		d.history = d.history[:n-1]
	} else {
		d.open(nil)
	}
	d.message = ""
}

func (d *dashboard) move(delta int) {
	switch {
	case d.confirm != nil:
		d.offset = clamp(d.offset+delta, 1)
	case d.screen != nil && len(d.screen.Choices) > 0:
		d.selection = clamp(d.selection+delta, len(d.screen.Choices)-1)
	case d.screen != nil && d.screen.Save == nil:
		d.offset = max(0, d.offset+delta)
	case d.screen != nil:
		d.selection = clamp(d.selection+delta, len(d.screen.Fields))
	default:
		d.selection = clamp(d.selection+delta, len(d.actions())-1)
	}
}

func clamp(n, high int) int { return max(0, min(n, high)) }

func (d *dashboard) KeyMap() tui.KeyMap {
	return tui.KeyMap{tui.OnStop(tui.AnyKey, d.key)}
}

func (d *dashboard) key(e tui.KeyEvent) {
	ctrlC := e.Key == tui.KeyRune && e.Mod.Has(tui.ModCtrl) && e.Rune == 'c'
	quit := e.Key == tui.KeyRune && e.Rune == 'q' && d.screen == nil
	if d.working == updateActionLabel && (ctrlC || quit || e.Key == tui.KeyEscape) {
		d.interruptUpdate(ctrlC || quit)
		return
	}
	if ctrlC {
		if d.working == "" {
			d.app.Stop()
		}
		return
	}
	if e.Key == tui.KeyRune && e.Rune == 't' && e.Mod == tui.ModCtrl {
		d.selectText = !d.selectText
		d.app.MarkDirty()
		return
	}
	if d.busy && d.working != "" && d.working != updateActionLabel {
		return
	}
	if d.clipboardKey(e) {
		d.app.MarkDirty()
		return
	}
	if e.Key == tui.KeyEscape || quit {
		d.back()
	} else {
		switch e.Key {
		case tui.KeyUp, tui.KeyTab:
			if e.Key == tui.KeyTab && !e.Mod.Has(tui.ModShift) {
				d.move(1)
			} else {
				d.move(-1)
			}
		case tui.KeyDown:
			d.move(1)
		case tui.KeyHome:
			d.move(-1 << 30)
		case tui.KeyEnd:
			d.move(1 << 30)
		case tui.KeyPageUp:
			d.move(-8)
		case tui.KeyPageDown:
			d.move(8)
		case tui.KeyEnter:
			d.activate()
		case tui.KeyBackspace, tui.KeyRune:
			if s := d.screen; s != nil && s.Save != nil && d.selection < len(s.Fields) {
				f := &s.Fields[d.selection]
				switch {
				case f.Toggle && e.Rune == ' ':
					d.activate()
				case s.Reorder && (e.Rune == '-' || e.Rune == '+' || e.Rune == '='):
					to := d.selection + 1
					if e.Rune == '-' {
						to = d.selection - 1
					}
					if to >= 0 && to < len(s.Fields) {
						s.Fields[d.selection], s.Fields[to] = s.Fields[to], s.Fields[d.selection]
						d.selection = to
					}
				case !f.Toggle && e.Key == tui.KeyBackspace && f.Value != "":
					_, size := utf8.DecodeLastRuneInString(f.Value)
					f.Value = f.Value[:len(f.Value)-size]
				case !f.Toggle && e.Rune == 'u' && e.Mod.Has(tui.ModCtrl):
					f.Value = ""
				case !f.Toggle && !e.Mod.Has(tui.ModCtrl|tui.ModAlt) && unicode.IsPrint(e.Rune) && len(f.Value) < maxFieldBytes:
					f.Value += string(e.Rune)
				}
			} else if e.Rune == ' ' && (d.screen == nil || len(d.screen.Choices) > 0) {
				d.activate()
			}
		}
	}
	d.app.MarkDirty()
}

func (d *dashboard) HandleMouse(e tui.MouseEvent) bool {
	if d.busy && d.working != "" && d.working != updateActionLabel {
		return false
	}
	if e.Button == tui.MouseWheelDown {
		d.move(3)
	} else if e.Button == tui.MouseWheelUp {
		d.move(-3)
	} else if e.Button == tui.MouseLeft && e.Action == tui.MousePress {
		for _, h := range d.hits {
			if h.element.Rect().Contains(e.X, e.Y) {
				if d.confirm != nil {
					d.offset = h.index
				} else {
					d.selection = h.index
				}
				if d.screen == nil || len(d.screen.Choices) > 0 || d.selection >= len(d.screen.Fields) || d.screen.Fields[d.selection].Toggle {
					d.activate()
				}
				d.app.MarkDirty()
				return true
			}
		}
		return false
	} else {
		return false
	}
	d.app.MarkDirty()
	return true
}

func (d *dashboard) Render(app *tui.App) *tui.Element {
	d.app = app
	d.syncInputMode(app.Terminal())
	d.hits = d.hits[:0]
	width, height := app.Size()
	root := tui.New(tui.WithDirection(tui.Column), tui.WithPaddingTRBL(0, 1, 0, 1))
	root.AddChild(line(d.menu.Unit.binaryName()+"  /  "+d.menu.Subtitle, heading))
	state := d.current.state
	if state == "" {
		state = "checking…"
		if d.current.err != nil {
			state = "unavailable"
		}
	}
	status := "Service: " + state
	if d.current.state != "" && state != NotInstalled {
		status += "   |   Autostart: " + checkbox(d.current.enabled)
	}
	switch hint := d.clipboardHint(); {
	case hint != "":
		status += "   |   " + hint
	case !d.mouseReleased:
		status += "   |   Ctrl+T: select text"
	case d.selectText:
		status += "   |   Ctrl+T: menu mouse"
	default:
		status += "   |   Use terminal to select / paste"
	}
	style := muted
	if state == "active" {
		style = good
	} else if state == "failed" || state == "unavailable" {
		style = warning
	}
	root.AddChild(line(status, style))
	showRelease := d.newRelease != "" && state != NotInstalled && d.screen == nil && d.confirm == nil
	if showRelease {
		root.AddChild(line("Update available: "+d.newRelease+" — use "+updateActionLabel, warning))
	}

	main := tui.New(tui.WithDirection(tui.Column), tui.WithFlexGrow(1), tui.WithOverflow(tui.OverflowHidden))
	root.AddChild(main)
	footer := "↑/↓ Move  •  Enter / Space Select  •  Esc Quit"
	msg := d.message
	if msg == "" && d.current.err != nil {
		msg = d.current.err.Error()
	}
	if msg != "" {
		msg = strings.Join(strings.Fields(msg), " ")
	}
	switch {
	case d.stoppingUpdate:
		msg = "GitHub update: waiting for safe stop…"
	case d.working == updateActionLabel:
		if progress, ok := d.updateProgress.Load().(string); ok {
			msg = "GitHub update: " + progress
		}
	case d.working != "":
		msg = d.working + "…"
	}
	rows := wrap(msg, max(1, width-2))
	room := max(1, height-4-min(len(rows), 3))
	if showRelease {
		room = max(1, room-1)
	}

	switch {
	case d.confirm != nil:
		main.AddChild(line(d.confirm.Label, heading), line(d.confirm.Confirm, neutral))
		choices := tui.New(tui.WithDirection(tui.Row), tui.WithGap(2))
		main.AddChild(choices)
		d.addChoice(choices, "Cancel", 0, d.offset == 0)
		d.addChoice(choices, "Confirm", 1, d.offset == 1)
		footer = "↑/↓ Choose  •  Enter Confirm  •  Esc Cancel"
	case d.screen != nil:
		s := d.screen
		main.AddChild(line(s.Title, heading))
		text := wrap(s.Text, max(1, width-2))
		if len(s.Choices) > 0 {
			helpLines := min(len(text), 2, max(0, room-2))
			for _, row := range text[:helpLines] {
				main.AddChild(line(row, muted))
			}
			from, to := d.window(len(s.Choices), max(1, room-1-helpLines))
			for i := from; i < to; i++ {
				d.addChoice(main, s.Choices[i].Label, i, i == d.selection)
			}
			footer = "↑/↓ Move  •  Enter / Space Select  •  Esc Back"
			if from > 0 || to < len(s.Choices) {
				footer = fmt.Sprintf("%d/%d  •  %s", d.selection+1, len(s.Choices), footer)
			}
		} else if s.Save == nil {
			limit := max(1, room-1)
			d.offset = clamp(d.offset, len(text)-limit)
			for _, row := range text[d.offset:min(len(text), d.offset+limit)] {
				main.AddChild(line(row, neutral))
			}
			footer = "↑/↓ Scroll  •  Enter / Esc Back"
			if s.Dismiss != nil {
				footer = "↑/↓ Scroll  •  Enter Acknowledge  •  Esc Keep for later"
			}
		} else {
			for _, row := range text[:min(len(text), 2)] {
				main.AddChild(line(row, muted))
			}
			d.renderForm(main, s, width, max(1, room-1-min(len(text), 2)))
			footer = "Tab / ↑/↓ Move  •  Enter Next / Save  •  Ctrl+U Clear  •  Esc Back"
			if s.Reorder {
				footer = "↑/↓ Select  •  Space Toggle  •  - / + or = Reorder  •  Enter Toggle / Save  •  Esc Back"
			}
		}
	default:
		if !d.root {
			main.AddChild(line("Read-only: reopen with sudo to manage the service.", warning))
			room--
		}
		items := d.actions()
		d.selection = clamp(d.selection, len(items)-1)
		from, to := d.window(len(items), max(1, room))
		for i := from; i < to; i++ {
			d.addChoice(main, items[i].Label, i, i == d.selection)
		}
		if from > 0 || to < len(items) {
			footer = fmt.Sprintf("%d/%d  •  %s", d.selection+1, len(items), footer)
		}
	}
	if d.working == updateActionLabel {
		footer = "↑/↓ Move  •  Esc Cancel download  •  Ctrl+C Quit safely"
	}

	if msg != "" {
		style := good
		if d.working == "" && (strings.HasPrefix(msg, "Error:") || msg == d.message && d.current.err != nil || d.message == "") {
			style = warning
		}
		for _, row := range rows[:min(3, len(rows))] {
			root.AddChild(line(row, style))
		}
	}
	root.AddChild(line(footer, muted))
	return root
}

func (d *dashboard) renderForm(root *tui.Element, s *Screen, width, room int) {
	from, to := d.window(len(s.Fields)+1, room)
	for i := from; i < to; i++ {
		if i == len(s.Fields) {
			d.addChoice(root, s.Submit, i, i == d.selection)
			continue
		}
		f := s.Fields[i]
		value := f.Value
		switch {
		case f.Toggle:
			value = checkbox(f.Checked)
		case f.Secret && value != "":
			value = fmt.Sprintf("[hidden, %d characters]", len(value))
		case !f.Secret:
			runes := []rune(value)
			if maxLen := max(1, width-len(f.Label)-8); len(runes) > maxLen {
				value = "…" + string(runes[len(runes)-maxLen:])
			}
		}
		if i == d.selection && !f.Toggle && !f.Secret {
			value += "▏"
		}
		d.addChoice(root, f.Label+": "+value, i, i == d.selection)
	}
}

func (d *dashboard) window(count, size int) (int, int) {
	d.offset = clamp(d.offset, count-size)
	d.offset = max(d.selection-size+1, min(d.offset, d.selection))
	return d.offset, min(count, d.offset+size)
}

var (
	neutral = tui.NewStyle()
	heading = neutral.Bold().Foreground(tui.Cyan)
	chosen  = neutral.Bold().Foreground(tui.Black).Background(tui.Cyan)
	muted   = neutral.Dim()
	good    = neutral.Foreground(tui.Green)
	warning = neutral.Foreground(tui.Yellow)
)

func line(text string, style tui.Style) *tui.Element {
	return tui.New(tui.WithText(text), tui.WithTextStyle(style), tui.WithHeight(1), tui.WithFlexShrink(0), tui.WithWrap(false))
}

func (d *dashboard) addChoice(root *tui.Element, label string, i int, selected bool) {
	prefix, style := "  ", neutral
	if selected {
		prefix, style = "> ", chosen
	}
	el := line(prefix+label+" ", style)
	root.AddChild(el)
	d.hits = append(d.hits, hit{el, i})
}

func checkbox(on bool) string {
	if on {
		return "[x]"
	}
	return "[ ]"
}

func wrap(text string, width int) []string {
	if text == "" {
		return nil
	}
	var out []string
	for raw := range strings.SplitSeq(text, "\n") {
		runes := []rune(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return ' '
			}
			return r
		}, raw))
		for len(runes) > width {
			out = append(out, string(runes[:width]))
			runes = runes[width:]
		}
		out = append(out, string(runes))
	}
	return out
}
