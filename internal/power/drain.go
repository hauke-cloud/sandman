package power

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/hauke-cloud/sandman/api/v1alpha1"
)

// mirrorPodAnnotation marks a pod the kubelet manages from a static manifest.
// There is no controller behind it to reschedule it, and deleting it through
// the API changes nothing on the node, so a drain leaves it alone.
const mirrorPodAnnotation = "kubernetes.io/config.mirror"

// drainOptions is the subset of DrainSpec the drain itself needs.
type drainOptions struct {
	ignoreDaemonSets   bool
	deleteEmptyDirData bool
	force              bool
	gracePeriodSeconds int64
}

// drainResult reports where a drain has got to. A drain is not run to
// completion in one call: each reconcile evicts what it can and reports what
// is left, so a slow eviction never blocks the controller.
type drainResult struct {
	// remaining is how many pods still have to leave the node.
	remaining int
	// evicted counts evictions issued by this pass.
	evicted int
	// blocked lists pods that cannot be evicted at all, with the reason.
	// These do not resolve by waiting.
	blocked []string
	// waiting lists pods whose eviction was refused for now, almost always by
	// a PodDisruptionBudget. These do resolve by waiting.
	waiting []string
}

// done reports whether the node is clear.
func (d drainResult) done() bool { return d.remaining == 0 }

// summary describes the state of the drain for the device's status message.
func (d drainResult) summary() string {
	switch {
	case d.done():
		return "node drained"
	case len(d.blocked) > 0:
		return fmt.Sprintf("%d pod(s) left, %d cannot be evicted: %s",
			d.remaining, len(d.blocked), joinFirst(d.blocked))
	case len(d.waiting) > 0:
		return fmt.Sprintf("%d pod(s) left, waiting on: %s", d.remaining, joinFirst(d.waiting))
	default:
		return fmt.Sprintf("%d pod(s) still terminating", d.remaining)
	}
}

// drainNode evicts what it can from the node and reports what is left.
//
// Pods are listed straight from the API server rather than from the informer
// cache: sandman has no reason to watch every pod in the cluster, and a drain
// working from a cache that is a few seconds stale would re-evict pods that
// are already gone.
func drainNode(
	ctx context.Context,
	c client.Client,
	reader client.Reader,
	device *v1alpha1.SandmanDevice,
	opts drainOptions,
	observe func(error),
) (drainResult, error) {
	nodeName := device.Spec.NodeRef.Name

	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.MatchingFields{"spec.nodeName": nodeName}); err != nil {
		return drainResult{}, fmt.Errorf("list pods on node %s: %w", nodeName, err)
	}

	var result drainResult
	for i := range pods.Items {
		pod := &pods.Items[i]

		switch action, reason := classifyPod(pod, device, opts); action {
		case podSkip:
			continue
		case podBlocked:
			result.remaining++
			result.blocked = append(result.blocked, fmt.Sprintf("%s/%s (%s)", pod.Namespace, pod.Name, reason))
		case podTerminating:
			result.remaining++
		case podEvict:
			result.remaining++
			if err := evictPod(ctx, c, pod, opts.gracePeriodSeconds); err != nil {
				if apierrors.IsNotFound(err) {
					// It left between the list and the eviction.
					result.remaining--
					continue
				}
				if apierrors.IsTooManyRequests(err) {
					// A PodDisruptionBudget refusing an eviction is the system
					// working. Wait for it rather than reaching for --force.
					result.waiting = append(result.waiting,
						fmt.Sprintf("%s/%s (disruption budget)", pod.Namespace, pod.Name))
					continue
				}
				observe(err)
				return result, fmt.Errorf("evict %s/%s: %w", pod.Namespace, pod.Name, err)
			}
			observe(nil)
			result.evicted++
		case podForceDelete:
			result.remaining++
			if err := deletePod(ctx, c, pod, opts.gracePeriodSeconds); err != nil && !apierrors.IsNotFound(err) {
				observe(err)
				return result, fmt.Errorf("delete %s/%s: %w", pod.Namespace, pod.Name, err)
			}
			observe(nil)
			result.evicted++
		}
	}

	sort.Strings(result.blocked)
	sort.Strings(result.waiting)
	return result, nil
}

type podAction int

const (
	podSkip podAction = iota
	podEvict
	podForceDelete
	podTerminating
	podBlocked
)

// classifyPod decides what a drain should do with one pod, and why.
func classifyPod(pod *corev1.Pod, device *v1alpha1.SandmanDevice, opts drainOptions) (action podAction, reason string) {
	// sandman's own shutdown pod is the one thing that must stay: evicting it
	// would cancel the very shutdown the drain is preparing for.
	if pod.Labels[v1alpha1.LabelDevice] == device.Name {
		return podSkip, "sandman shutdown pod"
	}
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return podSkip, "already finished"
	}
	if _, ok := pod.Annotations[mirrorPodAnnotation]; ok {
		return podSkip, "static pod"
	}
	if pod.DeletionTimestamp != nil {
		return podTerminating, "terminating"
	}

	owner := metav1.GetControllerOf(pod)
	if owner != nil && owner.Kind == "DaemonSet" {
		if opts.ignoreDaemonSets {
			// A DaemonSet pod is recreated on this node the moment it is
			// evicted, so waiting for it to leave never ends.
			return podSkip, "daemonset pod"
		}
		return podBlocked, "daemonset pod"
	}
	if !opts.deleteEmptyDirData && hasEmptyDir(pod) {
		return podBlocked, "emptyDir data"
	}
	if owner == nil {
		if !opts.force {
			return podBlocked, "no controller"
		}
		// Nothing will recreate it. It is lost either way -- the alternative
		// is losing it to a power cut instead of to a delete.
		return podForceDelete, "no controller"
	}
	return podEvict, ""
}

func hasEmptyDir(pod *corev1.Pod) bool {
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].EmptyDir != nil {
			return true
		}
	}
	return false
}

// evictPod asks the API server to evict a pod, which honours whatever
// PodDisruptionBudget covers it. That is the whole difference between draining
// a node and deleting pods off it.
func evictPod(ctx context.Context, c client.Client, pod *corev1.Pod, grace int64) error {
	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
	}
	if grace >= 0 {
		eviction.DeleteOptions = &metav1.DeleteOptions{GracePeriodSeconds: &grace}
	}
	return c.SubResource("eviction").Create(ctx, pod, eviction)
}

func deletePod(ctx context.Context, c client.Client, pod *corev1.Pod, grace int64) error {
	opts := []client.DeleteOption{}
	if grace >= 0 {
		opts = append(opts, client.GracePeriodSeconds(grace))
	}
	return c.Delete(ctx, pod, opts...)
}

// summaryLimit is how many pods a summary names before it starts counting.
// Three is enough to recognise a pattern and short enough to read in a status
// field.
const summaryLimit = 3

// joinFirst names the first few items and counts the rest.
func joinFirst(items []string) string {
	if len(items) <= summaryLimit {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(items[:summaryLimit], ", "), len(items)-summaryLimit)
}
