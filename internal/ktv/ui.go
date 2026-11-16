package ktv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/sahilm/fuzzy"
	utilexec "k8s.io/client-go/util/exec"
)

type stage int

const (
	stageNamespace stage = iota
	stagePod
	stageContainer
)

// item is a row in a list: a name to match on, plus extra display columns.
type item struct {
	name   string
	cols   []string
	status int // index into cols of a status to color, or -1
}

// list is the state of one stage's filterable list.
type list struct {
	items          []item
	matches        fuzzy.Matches
	query          string
	cursor, offset int
	loading        bool
}

// filter recomputes matches for query, keeping all items when it's empty.
func (l *list) filter(query string) {
	l.query = query
	l.matches = l.matches[:0]
	if query == "" {
		for i, it := range l.items {
			l.matches = append(l.matches, fuzzy.Match{Str: it.name, Index: i})
		}
	} else {
		l.matches = fuzzy.Find(query, names(l.items))
	}
	l.cursor = min(l.cursor, max(len(l.matches)-1, 0))
}

// scroll moves offset to keep the cursor within a window of h rows.
func (l *list) scroll(h int) {
	if l.cursor < l.offset {
		l.offset = l.cursor
	} else if l.cursor >= l.offset+h {
		l.offset = l.cursor - h + 1
	}
}

func names(items []item) []string {
	ns := make([]string, len(items))
	for i, it := range items {
		ns[i] = it.name
	}
	return ns
}

func (l *list) selected() (item, bool) {
	if len(l.matches) == 0 {
		return item{}, false
	}
	return l.items[l.matches[l.cursor].Index], true
}

var keys = struct {
	up, down, pgUp, pgDown, enter, back, quit, refresh, delete, cluster key.Binding
}{
	up:      key.NewBinding(key.WithKeys("up", "ctrl+p"), key.WithHelp("↑", "up")),
	down:    key.NewBinding(key.WithKeys("down", "ctrl+n"), key.WithHelp("↓", "down")),
	pgUp:    key.NewBinding(key.WithKeys("pgup")),
	pgDown:  key.NewBinding(key.WithKeys("pgdown")),
	enter:   binding("enter", "select"),
	back:    binding("esc", "back"),
	quit:    binding("ctrl+c", "quit"),
	refresh: binding("ctrl+r", "refresh"),
	delete:  binding("ctrl+d", "delete"),
	cluster: binding("ctrl+o", "cluster"),
}

var (
	titleStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("6")).Padding(0, 1)
	crumbStyle    = lipgloss.NewStyle().Bold(true)
	mutedStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	matchStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("5"))
	selectedStyle = lipgloss.NewStyle().Bold(true)
	cursorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	errorStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	okStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	warnStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
)

// newHelp returns a help model using the terminal's palette, as the default
// greys are too dim to read on dark backgrounds.
func newHelp() help.Model {
	h := help.New()
	h.Styles = help.Styles{ShortKey: cursorStyle, ShortSeparator: mutedStyle, Ellipsis: mutedStyle}
	return h
}

type model struct {
	client  *client
	stage   stage
	lists   [3]list
	ns, pod string  // selected namespace and pod
	pending *action // action waiting on a container to be chosen
	nsQuery string  // namespace given on the command line
	confirm bool    // awaiting y/n to delete the selected pod
	dark    bool    // terminal has a dark background
	view    *viewer // open viewer, if any

	showCluster bool     // show the cluster panel
	cluster     *cluster // loaded cluster info, if any
	clusterErr  error

	input         textinput.Model
	help          help.Model
	status        string
	width, height int
}

type itemsMsg struct {
	stage stage
	items []item
	err   error
}

type shellDoneMsg struct{ err error }

type clusterMsg struct {
	cluster cluster
	err     error
}

type deletedMsg struct {
	pod string
	err error
}

func newModel(c *client, nsQuery, podQuery string) model {
	in := textinput.New()
	in.Prompt = "❯ "
	in.Focus()

	m := model{client: c, nsQuery: nsQuery, dark: true, input: in, help: newHelp()}
	m.lists[stageNamespace].loading = true
	m.lists[stagePod].query = podQuery
	return m
}

func (m model) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, tea.RequestBackgroundColor, m.load(stageNamespace))
}

// load fetches the items for stage s.
func (m model) load(s stage) tea.Cmd {
	c, ns, pod := m.client, m.ns, m.pod
	return func() tea.Msg {
		ctx := context.Background()
		var items []item
		var err error
		switch s {
		case stageNamespace:
			items, err = c.namespaces(ctx)
		case stagePod:
			items, err = c.pods(ctx, ns)
		case stageContainer:
			items, err = c.containers(ctx, ns, pod)
		}
		return itemsMsg{stage: s, items: items, err: err}
	}
}

func (m model) loadCluster() tea.Cmd {
	c := m.client
	return func() tea.Msg {
		info, err := c.cluster(context.Background())
		return clusterMsg{cluster: info, err: err}
	}
}

func (m model) deletePod() tea.Cmd {
	c, ns, pod := m.client, m.ns, m.pod
	return func() tea.Msg {
		return deletedMsg{pod: pod, err: c.deletePod(context.Background(), ns, pod)}
	}
}

// enterAltScreen switches to the alternate screen and clears it.
const enterAltScreen = "\x1b[?1049h\x1b[H\x1b[2J"

// start runs a on the target, opening the viewer or, for interactive
// actions, releasing the terminal to it.
func (m model) start(a action, t target) (model, tea.Cmd) {
	if a.interactive != nil {
		// Bubble Tea leaves the alternate screen when releasing the terminal,
		// briefly revealing the shell's screen, so switch straight back.
		c := m.client
		return m, tea.Exec(execFunc(func() error {
			os.Stdout.WriteString(enterAltScreen)
			defer os.Stdout.WriteString(enterAltScreen)
			return a.interactive(c, context.Background(), t)
		}), func(err error) tea.Msg { return shellDoneMsg{err: err} })
	}

	title := a.key.Help().Desc
	if t.container != "" {
		title += " (" + t.container + ")"
	}
	ctx, cancel := context.WithCancel(context.Background())
	v := &viewer{id: 1, title: title, action: a, target: t, alt: a.alt, follow: a.follow, cancel: cancel, vp: viewport.New()}
	v.vp.SoftWrap = true
	if old := m.view; old != nil {
		old.close()
		v.id = old.id + 1
		if old.visible {
			// Keep showing the old text until the new arrives, e.g. when
			// switching to an alternative view.
			v.visible, v.stale, v.lines = true, true, old.lines
			v.vp.SoftWrap = old.vp.SoftWrap
			v.vp.SetContentLines(v.lines)
		}
	}
	m.view = v
	m.resizeView()
	m.status = mutedStyle.Render("loading " + title + "…")
	return m, openView(ctx, v.id, m.client, a, t, m.dark)
}

func (m *model) resizeView() {
	if m.view != nil {
		m.view.vp.SetWidth(m.width)
		m.view.vp.SetHeight(max(m.height-2, 1)) // title and footer
	}
}

func (m *model) closeView() {
	m.view.close()
	m.view = nil
	if m.stage == stageContainer {
		m.setStage(stagePod)
	}
}

// setStage switches to stage s, saving and restoring each stage's query.
func (m *model) setStage(s stage) {
	m.lists[m.stage].query = m.input.Value()
	m.stage = s
	m.input.SetValue(m.lists[s].query)
	m.input.CursorEnd()
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m, cmd := m.update(msg)
	m.lists[m.stage].scroll(m.listHeight())
	return m, cmd
}

func (m model) update(msg tea.Msg) (model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.input.SetWidth(msg.Width - 4)
		m.help.SetWidth(msg.Width)
		m.resizeView()
		return m, nil

	case tea.BackgroundColorMsg:
		m.dark = msg.IsDark()
		return m, nil

	case itemsMsg:
		return m.loaded(msg)

	case viewOpenedMsg:
		if m.view == nil || msg.id != m.view.id {
			if msg.closer != nil {
				msg.closer.Close()
			}
			return m, nil
		}
		if msg.err != nil {
			m.status = errorStyle.Render(msg.err.Error())
			m.closeView()
			return m, nil
		}
		m.status = ""
		m.view.closer, m.view.reader, m.view.visible = msg.closer, msg.r, true
		return m, readChunk(msg.id, msg.r)

	case viewChunkMsg:
		if m.view == nil || msg.id != m.view.id {
			return m, nil
		}
		m.view.append(msg.lines)
		if msg.err == nil {
			return m, readChunk(msg.id, m.view.reader)
		}
		m.view.done = true
		if !errors.Is(msg.err, io.EOF) && !errors.Is(msg.err, context.Canceled) {
			m.status = errorStyle.Render(msg.err.Error())
		}
		return m, nil

	case shellDoneMsg:
		var exitErr utilexec.ExitError
		if msg.err != nil && !errors.As(msg.err, &exitErr) {
			m.status = errorStyle.Render(msg.err.Error())
		}
		if m.stage == stageContainer {
			m.setStage(stagePod)
		}
		return m, m.load(stagePod)

	case clusterMsg:
		m.cluster, m.clusterErr = &msg.cluster, msg.err
		return m, nil

	case deletedMsg:
		if msg.err != nil {
			m.status = errorStyle.Render(msg.err.Error())
		} else {
			m.status = okStyle.Render("deleted pod " + msg.pod)
		}
		return m, m.load(stagePod)

	case tea.KeyPressMsg:
		if m.view != nil && m.view.visible {
			return m.viewKeyPress(msg)
		}
		return m.keyPress(msg)
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m model) viewKeyPress(msg tea.KeyPressMsg) (model, tea.Cmd) {
	switch {
	case key.Matches(msg, keys.quit):
		m.view.close()
		return m, tea.Quit
	case key.Matches(msg, viewKeys.close):
		m.closeView()
		return m, m.load(stagePod)
	case m.view.alt != nil && key.Matches(msg, m.view.alt.key):
		prev := m.view.action
		m, cmd := m.start(*m.view.alt, m.view.target)
		m.view.alt = &prev
		return m, cmd
	}
	return m, m.view.update(msg)
}

func (m model) loaded(msg itemsMsg) (model, tea.Cmd) {
	l := &m.lists[msg.stage]
	l.loading = false
	if msg.err != nil {
		m.status = errorStyle.Render(msg.err.Error())
		if msg.stage == stageContainer {
			m.pending = nil
		}
		return m, nil
	}
	l.items = msg.items
	l.filter(l.query)

	switch msg.stage {
	case stageNamespace:
		if ns, ok := autoMatch(m.nsQuery, names(msg.items)); ok {
			return m.selectNamespace(ns)
		}
		l.filter(m.nsQuery)
		m.input.SetValue(m.nsQuery)
		m.input.CursorEnd()

	case stageContainer:
		if len(msg.items) == 1 {
			a := m.pending
			m.pending = nil
			return m.start(*a, target{ns: m.ns, pod: m.pod, container: msg.items[0].name})
		}
		m.setStage(stageContainer)
	}
	return m, nil
}

func (m model) selectNamespace(ns string) (model, tea.Cmd) {
	m.ns = ns
	m.lists[stagePod].loading = true
	m.lists[stagePod].items = nil
	m.lists[stagePod].filter(m.lists[stagePod].query)
	m.setStage(stagePod)
	return m, m.load(stagePod)
}

func (m model) keyPress(msg tea.KeyPressMsg) (model, tea.Cmd) {
	if m.confirm {
		m.confirm = false
		m.status = ""
		if msg.String() == "y" {
			return m, m.deletePod()
		}
		return m, nil
	}

	l := &m.lists[m.stage]
	switch {
	case key.Matches(msg, keys.quit):
		return m, tea.Quit
	case key.Matches(msg, keys.back):
		m.status = ""
		switch m.stage {
		case stageNamespace:
			return m, tea.Quit
		case stagePod:
			m.setStage(stageNamespace)
		case stageContainer:
			m.pending = nil
			m.setStage(stagePod)
		}
		return m, nil
	case key.Matches(msg, keys.up):
		l.cursor = max(l.cursor-1, 0)
		return m, nil
	case key.Matches(msg, keys.down):
		l.cursor = min(l.cursor+1, max(len(l.matches)-1, 0))
		return m, nil
	case key.Matches(msg, keys.pgUp):
		l.cursor = max(l.cursor-m.listHeight(), 0)
		return m, nil
	case key.Matches(msg, keys.pgDown):
		l.cursor = min(l.cursor+m.listHeight(), max(len(l.matches)-1, 0))
		return m, nil
	case m.stage != stageContainer && key.Matches(msg, keys.cluster):
		m.showCluster = !m.showCluster
		if m.showCluster {
			return m, m.loadCluster()
		}
		return m, nil
	case m.stage != stageContainer && key.Matches(msg, keys.refresh):
		cmds := []tea.Cmd{m.load(m.stage)}
		if m.showCluster {
			cmds = append(cmds, m.loadCluster())
		}
		return m, tea.Batch(cmds...)
	}

	sel, ok := l.selected()
	switch m.stage {
	case stageNamespace:
		if key.Matches(msg, keys.enter) && ok {
			m.status = ""
			return m.selectNamespace(sel.name)
		}

	case stagePod:
		if key.Matches(msg, keys.delete) && ok {
			m.pod = sel.name
			m.confirm = true
			m.status = warnStyle.Render(fmt.Sprintf("delete pod %s? [y/N]", sel.name))
			return m, nil
		}
		for _, a := range actions {
			if !key.Matches(msg, a.key) {
				continue
			}
			if !ok {
				return m, nil
			}
			m.pod = sel.name
			m.status = ""
			if a.container {
				m.pending = &a
				m.lists[stageContainer] = list{loading: true}
				return m, m.load(stageContainer)
			}
			return m.start(a, target{ns: m.ns, pod: m.pod})
		}

	case stageContainer:
		if key.Matches(msg, keys.enter) && ok {
			a := m.pending
			m.pending = nil
			return m.start(*a, target{ns: m.ns, pod: m.pod, container: sel.name})
		}
	}

	// Anything else edits the filter.
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	if m.input.Value() != l.query {
		l.filter(m.input.Value())
	}
	return m, cmd
}

// listHeight is the number of rows available for list items.
func (m model) listHeight() int {
	const chrome = 6 // title, input, count, status, 2 help lines
	if m.height == 0 {
		return 10
	}
	return max(m.height-chrome, 1)
}

func (m model) View() tea.View {
	var content string
	if m.view != nil && m.view.visible {
		content = strings.Join([]string{m.titleView(), m.view.vp.View(), m.view.footer(m)}, "\n")
	} else {
		content = m.listView()
	}
	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

func (m model) listView() string {
	l := m.lists[m.stage]
	input := m.input
	var panel string
	if m.showCluster && m.stage != stageContainer {
		panel = m.clusterView(m.listHeight() + 2) // input, count and rows
		input.SetWidth(m.width - lipgloss.Width(panel) - 4)
	}
	body := []string{input.View()}

	if l.loading {
		body = append(body, mutedStyle.Render("  loading…"))
	} else {
		body = append(body, mutedStyle.Render(fmt.Sprintf("  %d/%d", len(l.matches), len(l.items))))
	}

	h := m.listHeight()
	widths := columnWidths(l.items)
	end := min(l.offset+h, len(l.matches))
	for i := l.offset; i < end; i++ {
		body = append(body, renderRow(l.items[l.matches[i].Index], l.matches[i].MatchedIndexes, widths, i == l.cursor))
	}
	for range h - (end - l.offset) {
		body = append(body, "")
	}

	content := strings.Join(body, "\n")
	if panel != "" {
		leftW := max(m.width-lipgloss.Width(panel), 0)
		for i, line := range body {
			body[i] = ansi.Truncate(line, leftW, "…")
		}
		left := lipgloss.NewStyle().Width(leftW).Render(strings.Join(body, "\n"))
		content = lipgloss.JoinHorizontal(lipgloss.Top, left, panel)
	}

	return strings.Join([]string{m.titleView(), content, " " + m.status, m.helpView()}, "\n")
}

// clusterView renders the cluster panel, height lines high and at most half
// the screen wide.
func (m model) clusterView(height int) string {
	lines := []string{crumbStyle.Render("cluster")}
	if c := m.cluster; c != nil {
		kv := func(k, v string) string { return mutedStyle.Render(fmt.Sprintf("%-8s", k)) + " " + v }
		lines = append(lines,
			kv("context", m.client.context),
			kv("server", c.server),
			kv("version", c.version),
			"",
			crumbStyle.Render(fmt.Sprintf("nodes (%d)", len(c.nodes))),
		)
		header := item{name: "NAME", cols: []string{"STATUS", "ROLES", "AGE", "VERSION"}, status: -1}
		widths := columnWidths(append([]item{header}, c.nodes...))
		lines = append(lines, renderRow(header, nil, widths, false))
		for _, n := range c.nodes {
			lines = append(lines, renderRow(n, nil, widths, false))
		}
	} else {
		lines = append(lines, mutedStyle.Render("loading…"))
	}
	if m.clusterErr != nil {
		lines = append(lines, errorStyle.Render(m.clusterErr.Error()))
	}

	maxW := max(m.width/2-2, 10) // less border and padding
	for i, line := range lines {
		lines[i] = ansi.Truncate(line, maxW, "…")
	}
	return lipgloss.NewStyle().
		Border(lipgloss.NormalBorder(), false, false, false, true).
		BorderForeground(lipgloss.Color("8")).
		PaddingLeft(1).
		Height(height).
		MaxHeight(height).
		Render(strings.Join(lines, "\n"))
}

func (m model) titleView() string {
	crumbs := []string{m.client.context, "namespaces"}
	switch {
	case m.view != nil && m.view.visible:
		crumbs = []string{m.client.context, m.ns, m.pod, m.view.title}
	case m.stage == stagePod:
		crumbs = []string{m.client.context, m.ns, "pods"}
	case m.stage == stageContainer:
		crumbs = []string{m.client.context, m.ns, m.pod, "containers"}
		if m.pending != nil {
			crumbs[3] += " (" + m.pending.key.Help().Desc + ")"
		}
	}
	return titleStyle.Render("ktv") + " " + crumbStyle.Render(strings.Join(crumbs, mutedStyle.Render(" › ")))
}

func (m model) helpView() string {
	nav := []key.Binding{keys.up, keys.down, keys.back, keys.quit}
	var top []key.Binding
	switch m.stage {
	case stageNamespace:
		top = []key.Binding{keys.enter}
		nav = append([]key.Binding{keys.cluster, keys.refresh}, nav...)
	case stagePod:
		for _, a := range actions {
			top = append(top, a.key)
		}
		nav = append([]key.Binding{keys.delete, keys.cluster, keys.refresh}, nav...)
	default:
		top = []key.Binding{keys.enter}
	}
	return m.help.ShortHelpView(top) + "\n" + m.help.ShortHelpView(nav)
}

// columnWidths returns the widths of the name and each extra column.
func columnWidths(items []item) []int {
	var widths []int
	for _, it := range items {
		for len(widths) < len(it.cols)+1 {
			widths = append(widths, 0)
		}
		widths[0] = max(widths[0], len(it.name))
		for i, c := range it.cols {
			widths[i+1] = max(widths[i+1], len(c))
		}
	}
	return widths
}

func renderRow(it item, matched []int, widths []int, selected bool) string {
	var b strings.Builder
	if selected {
		b.WriteString(cursorStyle.Render("▌ "))
	} else {
		b.WriteString("  ")
	}

	nameStyle := lipgloss.NewStyle()
	if selected {
		nameStyle = selectedStyle
	}
	hit := make(map[int]bool, len(matched))
	for _, i := range matched {
		hit[i] = true
	}
	for i, r := range it.name {
		if hit[i] {
			b.WriteString(matchStyle.Render(string(r)))
		} else {
			b.WriteString(nameStyle.Render(string(r)))
		}
	}
	b.WriteString(strings.Repeat(" ", widths[0]-len(it.name)))

	for i, c := range it.cols {
		pad := fmt.Sprintf("  %-*s", widths[i+1], c)
		if i == it.status {
			b.WriteString(statusStyle(c).Render(pad))
		} else {
			b.WriteString(mutedStyle.Render(pad))
		}
	}
	return b.String()
}

// statusStyle colors pod and namespace statuses by health.
func statusStyle(s string) lipgloss.Style {
	switch s {
	case "Running", "Active", "Succeeded", "Ready":
		return okStyle
	case "Completed":
		return mutedStyle
	case "Pending", "ContainerCreating", "PodInitializing", "Terminating":
		return warnStyle
	}
	if strings.HasPrefix(s, "Init:") || strings.HasPrefix(s, "Ready,") {
		return warnStyle
	}
	return errorStyle
}
