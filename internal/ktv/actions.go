package ktv

import (
	"context"
	"io"

	"charm.land/bubbles/v2/key"
	corev1 "k8s.io/api/core/v1"
)

// target is the pod (and container, for actions that need one) to act on.
type target struct {
	ns, pod, container string
}

// action is run on the selected pod. Most open text in the built-in viewer;
// interactive ones (like a shell) take over the terminal instead.
type action struct {
	key       key.Binding
	container bool // whether a container must be chosen first

	// open returns the text to view, colored as kubecolor colors the output
	// of the kubectl command color. If follow is set, the text streams.
	open   func(c *client, ctx context.Context, t target) (io.ReadCloser, error)
	color  []string
	follow bool

	interactive func(c *client, ctx context.Context, t target) error

	// alt is an alternative view, switched to from the viewer with its key
	// (and back again with this action's key).
	alt *action
}

var actions = []action{
	{key: binding("enter", "describe"), open: (*client).describe, color: []string{"describe", "pod"}},
	{key: binding("ctrl+l", "logs"), container: true, open: logs(corev1.PodLogOptions{}), color: []string{"logs"}},
	{key: binding("ctrl+f", "follow"), container: true, open: logs(corev1.PodLogOptions{Follow: true, TailLines: ptr(int64(1000))}), color: []string{"logs"}, follow: true},
	{key: binding("alt+l", "prev logs"), container: true, open: logs(corev1.PodLogOptions{Previous: true}), color: []string{"logs"}},
	{key: binding("ctrl+s", "shell"), container: true, interactive: (*client).shell},
	{key: binding("ctrl+y", "yaml"), open: podYAML(false), color: yamlColor, alt: &yamlFull},
	{key: binding("ctrl+v", "events"), open: (*client).events, color: []string{"get", "events"}},
}

func logs(opts corev1.PodLogOptions) func(c *client, ctx context.Context, t target) (io.ReadCloser, error) {
	return func(c *client, ctx context.Context, t target) (io.ReadCloser, error) {
		return c.logs(ctx, t, opts)
	}
}

func ptr[T any](v T) *T { return &v }

func binding(k, help string) key.Binding {
	return key.NewBinding(key.WithKeys(k), key.WithHelp(k, help))
}

// execFunc adapts a func to tea.ExecCommand, so it can run with the terminal
// released. The funcs use os.Stdin/os.Stdout directly.
type execFunc func() error

func (f execFunc) Run() error        { return f() }
func (execFunc) SetStdin(io.Reader)  {}
func (execFunc) SetStdout(io.Writer) {}
func (execFunc) SetStderr(io.Writer) {}

// yamlFull is the alternative to the YAML view's spec, showing everything.
var yamlFull = action{key: binding("y", "full yaml"), open: podYAML(true), color: yamlColor}

var yamlColor = []string{"get", "pod", "-o", "yaml"}

func podYAML(full bool) func(c *client, ctx context.Context, t target) (io.ReadCloser, error) {
	return func(c *client, ctx context.Context, t target) (io.ReadCloser, error) {
		return c.yaml(ctx, t, full)
	}
}
