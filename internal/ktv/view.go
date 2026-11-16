package ktv

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
)

// viewer is the built-in pager, showing an action's (possibly streaming) text.
type viewer struct {
	id     int // distinguishes messages from a previous viewer
	title  string
	action action
	target target
	alt    *action // view to switch to, if any
	follow bool    // keep scrolled to the bottom as text arrives

	visible bool // shown, once opened or while replacing a visible viewer
	stale   bool // showing a previous viewer's text until the first chunk
	done    bool

	vp     viewport.Model
	lines  []string
	reader *bufio.Reader
	cancel context.CancelFunc
	closer io.Closer
}

type viewOpenedMsg struct {
	id     int
	r      *bufio.Reader
	closer io.Closer
	err    error
}

// viewChunkMsg carries lines read from the viewer's text; err is io.EOF at
// the end.
type viewChunkMsg struct {
	id    int
	lines []string
	err   error
}

var viewKeys = struct {
	top, bottom, wrap, follow, close key.Binding
}{
	top:    key.NewBinding(key.WithKeys("g", "home"), key.WithHelp("g", "top")),
	bottom: key.NewBinding(key.WithKeys("G", "end"), key.WithHelp("G", "bottom")),
	wrap:   binding("w", "wrap"),
	follow: binding("F", "follow"),
	close:  key.NewBinding(key.WithKeys("esc", "q"), key.WithHelp("esc", "back")),
}

// openView starts a's text for t, colored for a dark or light background.
func openView(ctx context.Context, id int, c *client, a action, t target, dark bool) tea.Cmd {
	return func() tea.Msg {
		src, err := a.open(c, ctx, t)
		if err != nil {
			return viewOpenedMsg{id: id, err: err}
		}
		r, err := colorize(a.color, dark, src)
		if err != nil {
			src.Close()
			return viewOpenedMsg{id: id, err: err}
		}
		return viewOpenedMsg{id: id, r: bufio.NewReader(r), closer: multiCloser{r, src}}
	}
}

// readChunk reads the next line, plus any more already buffered.
func readChunk(id int, r *bufio.Reader) tea.Cmd {
	return func() tea.Msg {
		var lines []string
		for {
			line, err := r.ReadString('\n')
			if line != "" {
				lines = append(lines, cleanLine(line))
			}
			if err != nil {
				return viewChunkMsg{id: id, lines: lines, err: err}
			}
			if r.Buffered() == 0 || len(lines) >= 5000 {
				return viewChunkMsg{id: id, lines: lines}
			}
		}
	}
}

// cleanLine strips the line ending and expands tabs, which the viewport
// doesn't measure.
func cleanLine(s string) string {
	s = strings.TrimRight(s, "\r\n")
	return strings.ReplaceAll(s, "\t", "    ")
}

type multiCloser []io.Closer

func (mc multiCloser) Close() error {
	var errs []error
	for _, c := range mc {
		errs = append(errs, c.Close())
	}
	return errors.Join(errs...)
}

func (v *viewer) close() {
	if v.cancel != nil {
		v.cancel()
	}
	if v.closer != nil {
		v.closer.Close()
	}
}

// append adds lines, keeping to the bottom if following.
func (v *viewer) append(lines []string) {
	if v.stale {
		v.stale, v.lines = false, nil
		v.vp.GotoTop()
	}
	v.lines = append(v.lines, lines...)
	v.vp.SetContentLines(v.lines)
	if v.follow {
		v.vp.GotoBottom()
	}
}

func (v *viewer) update(msg tea.KeyPressMsg) tea.Cmd {
	switch {
	case key.Matches(msg, viewKeys.top):
		v.follow = false
		v.vp.GotoTop()
	case key.Matches(msg, viewKeys.bottom):
		v.vp.GotoBottom()
	case key.Matches(msg, viewKeys.wrap):
		v.vp.SoftWrap = !v.vp.SoftWrap
	case key.Matches(msg, viewKeys.follow):
		v.follow = !v.follow
		if v.follow {
			v.vp.GotoBottom()
		}
	default:
		var cmd tea.Cmd
		v.vp, cmd = v.vp.Update(msg)
		if !v.vp.AtBottom() {
			v.follow = false
		}
		return cmd
	}
	return nil
}

func (v *viewer) footer(m model) string {
	state := fmt.Sprintf("%3.0f%%", v.vp.ScrollPercent()*100)
	if !v.done {
		state += " • streaming"
	}
	if v.follow {
		state += " • following"
	}
	bindings := []key.Binding{viewKeys.close, viewKeys.top, viewKeys.bottom, viewKeys.wrap, viewKeys.follow}
	if v.alt != nil {
		bindings = append(bindings, v.alt.key)
	}
	help := m.help.ShortHelpView(append(bindings, keys.quit))
	return help + "  " + state
}
