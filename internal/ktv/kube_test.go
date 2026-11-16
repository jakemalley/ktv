package ktv

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func testPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "etcd-node1", Namespace: "kube-system",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)),
			ManagedFields:     []metav1.ManagedFieldsEntry{{Manager: "kubelet"}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "etcd"}, {Name: "sidecar"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "etcd", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
				{Name: "sidecar", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
			},
		},
	}
}

func testClient() *client {
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "e1", Namespace: "kube-system"},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "etcd-node1", Namespace: "kube-system"},
		Type:           "Warning", Reason: "BackOff", Message: "Back-off restarting failed container",
		LastTimestamp: metav1.NewTime(time.Now().Add(-time.Minute)),
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system"}, Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive}}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node1", Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""}},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			NodeInfo:   corev1.NodeSystemInfo{KubeletVersion: "v1.31.0"},
		},
	}
	return &client{cs: fake.NewClientset(ns, node, testPod(), ev), cfg: &rest.Config{Host: "https://example:6443"}, context: "test"}
}

func TestPodStatus(t *testing.T) {
	waiting := func(reason string) corev1.ContainerState {
		return corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}
	}

	crash := testPod()
	crash.Status.ContainerStatuses[0] = corev1.ContainerStatus{
		RestartCount: 3, State: waiting("CrashLoopBackOff"),
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{FinishedAt: metav1.NewTime(time.Now().Add(-5 * time.Minute))}},
	}

	initWaiting := testPod()
	initWaiting.Spec.InitContainers = []corev1.Container{{Name: "init"}}
	initWaiting.Status.InitContainerStatuses = []corev1.ContainerStatus{{State: waiting("ImagePullBackOff")}}

	initRunning := testPod()
	initRunning.Spec.InitContainers = []corev1.Container{{Name: "a"}, {Name: "b"}}
	initRunning.Status.InitContainerStatuses = []corev1.ContainerStatus{
		{State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
		{State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
	}

	terminating := testPod()
	terminating.DeletionTimestamp = ptr(metav1.Now())

	tests := []struct {
		name                    string
		pod                     *corev1.Pod
		ready, status, restarts string
	}{
		{"running", testPod(), "2/2", "Running", "0"},
		{"crashloop", crash, "1/2", "CrashLoopBackOff", "3 (5m ago)"},
		{"init waiting", initWaiting, "0/2", "Init:ImagePullBackOff", "0"},
		{"init running", initRunning, "0/2", "Init:1/2", "0"},
		{"terminating", terminating, "2/2", "Terminating", "0"},
	}
	for _, tt := range tests {
		ready, status, restarts := podStatus(tt.pod)
		if ready != tt.ready || status != tt.status || restarts != tt.restarts {
			t.Errorf("%s: got %s %s %q; want %s %s %q", tt.name, ready, status, restarts, tt.ready, tt.status, tt.restarts)
		}
	}
}
