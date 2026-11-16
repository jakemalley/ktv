package ktv

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// send updates m with msg, then runs the resulting command and any it chains
// to (e.g. loading, then reading a viewer's text), skipping batches.
func send(t *testing.T, m model, msg tea.Msg) model {
	t.Helper()
	for msg != nil {
		next, cmd := m.Update(msg)
		m = next.(model)
		if cmd == nil {
			return m
		}
		msg = cmd()
		if _, ok := msg.(tea.BatchMsg); ok {
			return m
		}
	}
	return m
}

func TestUIDescribe(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := newModel(testClient(), "kube", "etcd")
	m = send(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	m = send(t, m, m.load(stageNamespace)())

	if m.stage != stagePod || m.ns != "kube-system" {
		t.Fatalf("stage = %v, ns = %q; want pods in kube-system", m.stage, m.ns)
	}
	if v := ansi.Strip(m.View().Content); !strings.Contains(v, "etcd-node1") || !strings.Contains(v, "Running") {
		t.Fatalf("pod list view missing pod:\n%s", v)
	}

	m = send(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.view == nil || !m.view.done {
		t.Fatal("viewer not opened and read to the end")
	}
	if v := ansi.Strip(m.View().Content); !strings.Contains(v, "Name:") || !strings.Contains(v, "describe") {
		t.Fatalf("viewer missing describe output:\n%s", v)
	}

	m = send(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.view != nil || m.stage != stagePod {
		t.Fatal("esc didn't close the viewer")
	}

	m = send(t, m, tea.KeyPressMsg{Code: 'v', Mod: tea.ModCtrl})
	if all := strings.Join(m.view.lines, "\n"); !strings.Contains(all, "LAST SEEN") || !strings.Contains(all, "BackOff") {
		t.Fatalf("events view = %q", all)
	}
}

func TestUILogsPicksContainer(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := newModel(testClient(), "kube-system", "")
	m = send(t, m, m.load(stageNamespace)())

	m = send(t, m, tea.KeyPressMsg{Code: 'l', Mod: tea.ModCtrl})
	if m.stage != stageContainer || len(m.lists[stageContainer].items) != 2 {
		t.Fatalf("stage = %v; want container picker with 2 containers", m.stage)
	}

	m = send(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.view == nil || m.view.title != "logs (etcd)" || len(m.view.lines) == 0 {
		t.Fatalf("viewer = %+v; want logs for etcd", m.view)
	}
}

func TestUIClusterPanel(t *testing.T) {
	m := newModel(testClient(), "", "")
	m = send(t, m, tea.WindowSizeMsg{Width: 140, Height: 30})
	m = send(t, m, m.load(stageNamespace)())

	m = send(t, m, tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	v := ansi.Strip(m.View().Content)
	for _, want := range []string{"kube-system", "cluster", "https://example:6443", "nodes (1)", "node1", "Ready", "control-plane", "v1.31.0"} {
		if !strings.Contains(v, want) {
			t.Errorf("view missing %q:\n%s", want, v)
		}
	}

	m = send(t, m, tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	if v := ansi.Strip(m.View().Content); strings.Contains(v, "nodes (1)") {
		t.Errorf("panel still shown after toggling off:\n%s", v)
	}
}

func TestUIYAMLToggle(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	m := newModel(testClient(), "kube-system", "")
	m = send(t, m, m.load(stageNamespace)())

	m = send(t, m, tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl})
	if m.view == nil || m.view.lines[0] != "spec:" || strings.Contains(strings.Join(m.view.lines, "\n"), "status:") {
		t.Fatalf("yaml view = %+v; want spec only", m.view)
	}

	m = send(t, m, tea.KeyPressMsg{Code: 'y', Text: "y"})
	if all := strings.Join(m.view.lines, "\n"); m.view.title != "full yaml" || !strings.Contains(all, "kind: Pod") || !strings.Contains(all, "status:") || strings.Contains(all, "managedFields") {
		t.Fatalf("after y, view = %q:\n%s", m.view.title, all)
	}

	m = send(t, m, tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl})
	if m.view.title != "yaml" || m.view.lines[0] != "spec:" {
		t.Fatalf("after ctrl+y, view = %q; want spec again", m.view.title)
	}
}
