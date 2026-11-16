package ktv

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/kubectl/pkg/describe"
	kterm "k8s.io/kubectl/pkg/util/term"
	"k8s.io/streaming/pkg/httpstream"
	"sigs.k8s.io/yaml"
)

// client talks to the cluster of the current kubeconfig context.
type client struct {
	cs      kubernetes.Interface
	cfg     *rest.Config
	context string
}

func newClient() (*client, error) {
	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{})
	raw, err := cc.RawConfig()
	if err != nil {
		return nil, err
	}
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &client{cs: cs, cfg: cfg, context: raw.CurrentContext}, nil
}

func (c *client) namespaces(ctx context.Context) ([]item, error) {
	list, err := c.cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	items := make([]item, len(list.Items))
	for i, ns := range list.Items {
		items[i] = item{name: ns.Name, cols: []string{string(ns.Status.Phase), age(ns.CreationTimestamp)}, status: 0}
	}
	return items, nil
}

func (c *client) pods(ctx context.Context, ns string) ([]item, error) {
	list, err := c.cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	items := make([]item, len(list.Items))
	for i := range list.Items {
		p := &list.Items[i]
		ready, status, restarts := podStatus(p)
		items[i] = item{name: p.Name, cols: []string{ready, status, restarts, age(p.CreationTimestamp)}, status: 1}
	}
	return items, nil
}

// cluster is a summary of the cluster, with its nodes as "kubectl get nodes"
// shows them.
type cluster struct {
	server, version string
	nodes           []item
}

func (c *client) cluster(ctx context.Context) (cluster, error) {
	info := cluster{server: c.cfg.Host}
	if v, err := c.cs.Discovery().ServerVersion(); err == nil {
		info.version = v.GitVersion
	}

	list, err := c.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return info, err
	}
	for _, n := range list.Items {
		status := "NotReady"
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				status = "Ready"
			}
		}
		if n.Spec.Unschedulable {
			status += ",SchedulingDisabled"
		}
		var roles []string
		for l := range n.Labels {
			if role, ok := strings.CutPrefix(l, "node-role.kubernetes.io/"); ok && role != "" {
				roles = append(roles, role)
			}
		}
		slices.Sort(roles)
		info.nodes = append(info.nodes, item{
			name:   n.Name,
			cols:   []string{status, cmp.Or(strings.Join(roles, ","), "<none>"), age(n.CreationTimestamp), n.Status.NodeInfo.KubeletVersion},
			status: 0,
		})
	}
	return info, nil
}

func (c *client) containers(ctx context.Context, ns, pod string) ([]item, error) {
	p, err := c.cs.CoreV1().Pods(ns).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	items := make([]item, len(p.Spec.Containers))
	for i, ct := range p.Spec.Containers {
		items[i] = item{name: ct.Name, status: -1}
	}
	return items, nil
}

func (c *client) describe(ctx context.Context, t target) (io.ReadCloser, error) {
	d := &describe.PodDescriber{Interface: c.cs}
	out, err := d.Describe(t.ns, t.pod, describe.DescriberSettings{ShowEvents: true, ChunkSize: 500})
	if err != nil {
		return nil, err
	}
	return io.NopCloser(strings.NewReader(out)), nil
}

// yaml returns the pod as YAML: just its spec, or if full, everything as
// "kubectl get -o yaml" shows it.
func (c *client) yaml(ctx context.Context, t target, full bool) (io.ReadCloser, error) {
	p, err := c.cs.CoreV1().Pods(t.ns).Get(ctx, t.pod, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	var obj any = map[string]any{"spec": p.Spec}
	if full {
		// Typed, without managed fields, as kubectl does.
		p.APIVersion, p.Kind = "v1", "Pod"
		p.ManagedFields = nil
		obj = p
	}
	out, err := yaml.Marshal(obj)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(out)), nil
}

func (c *client) events(ctx context.Context, t target) (io.ReadCloser, error) {
	sel := fields.Set{"involvedObject.kind": "Pod", "involvedObject.name": t.pod}.AsSelector().String()
	list, err := c.cs.CoreV1().Events(t.ns).List(ctx, metav1.ListOptions{FieldSelector: sel})
	if err != nil {
		return nil, err
	}
	evs := list.Items
	slices.SortFunc(evs, func(a, b corev1.Event) int { return lastSeen(a).Compare(lastSeen(b)) })

	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 8, 3, ' ', 0)
	fmt.Fprintln(w, "LAST SEEN\tTYPE\tREASON\tMESSAGE")
	for _, e := range evs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", duration.HumanDuration(time.Since(lastSeen(e))), e.Type, e.Reason, strings.TrimSpace(e.Message))
	}
	w.Flush()
	return io.NopCloser(strings.NewReader(b.String())), nil
}

func lastSeen(e corev1.Event) time.Time {
	switch {
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}

func (c *client) logs(ctx context.Context, t target, opts corev1.PodLogOptions) (io.ReadCloser, error) {
	opts.Container = t.container
	return c.cs.CoreV1().Pods(t.ns).GetLogs(t.pod, &opts).Stream(ctx)
}

func (c *client) deletePod(ctx context.Context, ns, pod string) error {
	return c.cs.CoreV1().Pods(ns).Delete(ctx, pod, metav1.DeleteOptions{})
}

// shell runs an interactive shell in the container, using the terminal.
func (c *client) shell(ctx context.Context, t target) error {
	req := c.cs.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(t.ns).Name(t.pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: t.container,
			Command:   []string{"sh", "-c", "command -v bash >/dev/null && exec bash || exec sh"},
			Stdin:     true,
			Stdout:    true,
			TTY:       true,
		}, scheme.ParameterCodec)

	// As kubectl does: websockets, falling back to SPDY.
	spdy, err := remotecommand.NewSPDYExecutor(c.cfg, "POST", req.URL())
	if err != nil {
		return err
	}
	ws, err := remotecommand.NewWebSocketExecutor(c.cfg, "GET", req.URL().String())
	if err != nil {
		return err
	}
	exec, err := remotecommand.NewFallbackExecutor(ws, spdy, func(err error) bool {
		return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
	})
	if err != nil {
		return err
	}

	tty := kterm.TTY{In: os.Stdin, Out: os.Stdout, Raw: true}
	sizes := sizeQueue{tty.MonitorSize(tty.GetSize())}
	return tty.Safe(func() error {
		return exec.StreamWithContext(ctx, remotecommand.StreamOptions{
			Stdin:             os.Stdin,
			Stdout:            os.Stdout,
			Tty:               true,
			TerminalSizeQueue: sizes,
		})
	})
}

func age(t metav1.Time) string {
	return duration.HumanDuration(time.Since(t.Time))
}

// podStatus returns the READY, STATUS and RESTARTS columns as shown by
// "kubectl get pods", following kubectl's (simplified) rules.
func podStatus(p *corev1.Pod) (ready, status, restarts string) {
	reason := cmp.Or(p.Status.Reason, string(p.Status.Phase))
	var restartCount, readyCount int32
	var lastRestart time.Time
	noteRestart := func(cs corev1.ContainerStatus) {
		restartCount += cs.RestartCount
		if t := cs.LastTerminationState.Terminated; t != nil && t.FinishedAt.After(lastRestart) {
			lastRestart = t.FinishedAt.Time
		}
	}

	initializing := false
	for i, cs := range p.Status.InitContainerStatuses {
		noteRestart(cs)
		switch {
		case cs.State.Terminated != nil && cs.State.Terminated.ExitCode == 0:
			continue
		case cs.State.Terminated != nil:
			reason = "Init:" + cmp.Or(cs.State.Terminated.Reason, fmt.Sprintf("ExitCode:%d", cs.State.Terminated.ExitCode))
		case cs.State.Waiting != nil && cs.State.Waiting.Reason != "" && cs.State.Waiting.Reason != "PodInitializing":
			reason = "Init:" + cs.State.Waiting.Reason
		default:
			reason = fmt.Sprintf("Init:%d/%d", i, len(p.Spec.InitContainers))
		}
		initializing = true
		break
	}

	if !initializing {
		restartCount, lastRestart = 0, time.Time{}
		hasRunning := false
		for _, cs := range slices.Backward(p.Status.ContainerStatuses) {
			noteRestart(cs)
			switch {
			case cs.State.Waiting != nil && cs.State.Waiting.Reason != "":
				reason = cs.State.Waiting.Reason
			case cs.State.Terminated != nil:
				reason = cmp.Or(cs.State.Terminated.Reason, fmt.Sprintf("ExitCode:%d", cs.State.Terminated.ExitCode))
			case cs.Ready && cs.State.Running != nil:
				hasRunning = true
				readyCount++
			}
		}
		if reason == "Completed" && hasRunning {
			reason = "Running"
		}
	}

	if p.DeletionTimestamp != nil {
		reason = "Terminating"
	}

	restarts = fmt.Sprint(restartCount)
	if !lastRestart.IsZero() {
		restarts += fmt.Sprintf(" (%s ago)", duration.HumanDuration(time.Since(lastRestart)))
	}
	return fmt.Sprintf("%d/%d", readyCount, len(p.Spec.Containers)), reason, restarts
}

// sizeQueue adapts kubectl's terminal size queue to remotecommand's.
type sizeQueue struct{ kterm.TerminalSizeQueue }

func (q sizeQueue) Next() *remotecommand.TerminalSize {
	s := q.TerminalSizeQueue.Next()
	if s == nil {
		return nil
	}
	return &remotecommand.TerminalSize{Width: s.Width, Height: s.Height}
}
