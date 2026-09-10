package power

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/hauke-cloud/sandman/api/v1alpha1"
)

func pod(mutators ...func(*corev1.Pod)) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "workload", Namespace: "default"},
		Spec:       corev1.PodSpec{NodeName: nodeName},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	// A pod with a controller behind it is the ordinary case, so it is the
	// baseline every case here varies from.
	p.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "apps/v1",
		Kind:       "ReplicaSet",
		Name:       "workload-abc",
		UID:        "1",
		Controller: ptr.To(true),
	}}
	for _, mutate := range mutators {
		mutate(p)
	}
	return p
}

func TestClassifyPod(t *testing.T) {
	device := newDevice()
	permissive := drainOptions{ignoreDaemonSets: true, deleteEmptyDirData: true, force: true}
	strict := drainOptions{}

	tests := []struct {
		name string
		pod  *corev1.Pod
		opts drainOptions
		want podAction
	}{
		{
			name: "ordinary workload is evicted",
			pod:  pod(),
			opts: permissive,
			want: podEvict,
		},
		{
			name: "sandman's own shutdown pod is left alone",
			pod: pod(func(p *corev1.Pod) {
				p.Labels = map[string]string{v1alpha1.LabelDevice: deviceName}
			}),
			opts: permissive,
			want: podSkip,
		},
		{
			name: "finished pod is left alone",
			pod:  pod(func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }),
			opts: permissive,
			want: podSkip,
		},
		{
			name: "static pod is left alone",
			pod: pod(func(p *corev1.Pod) {
				p.Annotations = map[string]string{mirrorPodAnnotation: "x"}
			}),
			opts: permissive,
			want: podSkip,
		},
		{
			name: "terminating pod is only waited for",
			pod: pod(func(p *corev1.Pod) {
				now := metav1.Now()
				p.DeletionTimestamp = &now
				p.Finalizers = []string{"holding"}
			}),
			opts: permissive,
			want: podTerminating,
		},
		{
			name: "daemonset pod is skipped when ignored",
			pod: pod(func(p *corev1.Pod) {
				p.OwnerReferences[0].Kind = "DaemonSet"
			}),
			opts: permissive,
			want: podSkip,
		},
		{
			name: "daemonset pod blocks when not ignored",
			pod: pod(func(p *corev1.Pod) {
				p.OwnerReferences[0].Kind = "DaemonSet"
			}),
			opts: strict,
			want: podBlocked,
		},
		{
			name: "emptyDir blocks unless its data may be discarded",
			pod: pod(func(p *corev1.Pod) {
				p.Spec.Volumes = []corev1.Volume{{
					Name:         "scratch",
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
				}}
			}),
			opts: strict,
			want: podBlocked,
		},
		{
			name: "uncontrolled pod blocks without force",
			pod:  pod(func(p *corev1.Pod) { p.OwnerReferences = nil }),
			opts: drainOptions{ignoreDaemonSets: true, deleteEmptyDirData: true},
			want: podBlocked,
		},
		{
			name: "uncontrolled pod is deleted with force",
			pod:  pod(func(p *corev1.Pod) { p.OwnerReferences = nil }),
			opts: permissive,
			want: podForceDelete,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := classifyPod(tc.pod, device, tc.opts)
			if got != tc.want {
				t.Fatalf("classifyPod = %v (%s), want %v", got, reason, tc.want)
			}
		})
	}
}

func TestDrainResultSummary(t *testing.T) {
	if got := (drainResult{}).summary(); got != "node drained" {
		t.Fatalf("summary of an empty node = %q", got)
	}

	blocked := drainResult{remaining: 2, blocked: []string{"default/a (no controller)", "default/b (emptyDir data)"}}
	if got := blocked.summary(); !strings.Contains(got, "cannot be evicted") {
		t.Fatalf("a blocked drain must say so, got %q", got)
	}

	waiting := drainResult{remaining: 1, waiting: []string{"default/a (disruption budget)"}}
	if got := waiting.summary(); !strings.Contains(got, "waiting on") {
		t.Fatalf("a drain held up by a budget must say what it is waiting on, got %q", got)
	}

	terminating := drainResult{remaining: 3}
	if got := terminating.summary(); !strings.Contains(got, "terminating") {
		t.Fatalf("pods on their way out should be reported as such, got %q", got)
	}
}

func TestJoinFirstTruncates(t *testing.T) {
	items := []string{"a", "b", "c", "d", "e"}
	got := joinFirst(items)
	if got != "a, b, c and 2 more" {
		t.Fatalf("joinFirst = %q", got)
	}
	if got := joinFirst(items[:2]); got != "a, b" {
		t.Fatalf("joinFirst under the limit = %q", got)
	}
}
