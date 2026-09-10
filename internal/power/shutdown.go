package power

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/hauke-cloud/sandman/api/v1alpha1"
)

// maxNamePrefix keeps the generated pod name inside the 63-character limit
// once Kubernetes appends its random suffix.
const maxNamePrefix = 45

// buildShutdownPod returns the privileged pod that powers a machine off.
//
// The pod is pinned with spec.nodeName rather than scheduled. By the time it
// runs, the node is cordoned and may already carry NoSchedule taints from the
// drain, so a scheduled pod would sit Pending forever; naming the node skips
// the scheduler entirely, and tolerating everything keeps the kubelet from
// turning it away.
func buildShutdownPod(device *v1alpha1.SandmanDevice, namespace string) *corev1.Pod {
	node := device.Spec.NodeRef.Name
	shutdown := &device.Spec.Shutdown

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("sandman-poweroff-%s-", truncate(node, maxNamePrefix)),
			Namespace:    namespace,
			Labels: map[string]string{
				v1alpha1.LabelDevice:           device.Name,
				v1alpha1.LabelNode:             node,
				"app.kubernetes.io/name":       "sandman",
				"app.kubernetes.io/component":  "shutdown",
				"app.kubernetes.io/managed-by": "sandman",
			},
			Annotations: map[string]string{
				"sandman.hauke.cloud/reason": "powering off " + node + " on request",
			},
		},
		Spec: corev1.PodSpec{
			// Not scheduled: placed.
			NodeName:      node,
			RestartPolicy: corev1.RestartPolicyNever,
			// The host's PID namespace is what makes `nsenter -t 1` find the
			// host's systemd rather than the container's own PID 1.
			HostPID:                       true,
			Tolerations:                   shutdown.PodTolerations(),
			ActiveDeadlineSeconds:         ptr.To(shutdown.Deadline()),
			TerminationGracePeriodSeconds: ptr.To[int64](5),
			PriorityClassName:             shutdown.PriorityClassName,
			// It talks to nothing, so it gets no credentials.
			AutomountServiceAccountToken: ptr.To(false),
			Containers: []corev1.Container{{
				Name:    "poweroff",
				Image:   shutdown.ContainerImage(),
				Command: shutdown.ShutdownCommand(),
				SecurityContext: &corev1.SecurityContext{
					// Entering another namespace needs the capability set that
					// only a privileged container has. This is the one place
					// in sandman that asks for it, and it lasts as long as the
					// poweroff takes.
					Privileged: ptr.To(true),
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resourceQuantity("10m"),
						corev1.ResourceMemory: resourceQuantity("16Mi"),
					},
				},
			}},
		},
	}
}

// createShutdownPod creates the pod and returns its name.
func createShutdownPod(
	ctx context.Context,
	c client.Client,
	scheme *runtime.Scheme,
	device *v1alpha1.SandmanDevice,
	namespace string,
) (string, error) {
	pod := buildShutdownPod(device, namespace)

	// A cluster-scoped owner may own a namespaced dependent, so the pod is
	// garbage collected with its device. It is not the main cleanup path --
	// sandman deletes the pod once the machine is off -- but it catches the
	// case where a device is deleted mid-shutdown.
	if err := controllerutil.SetControllerReference(device, pod, scheme); err != nil {
		return "", fmt.Errorf("set owner reference: %w", err)
	}
	if err := c.Create(ctx, pod); err != nil {
		return "", fmt.Errorf("create shutdown pod in namespace %s: %w", namespace, err)
	}
	return pod.Name, nil
}

// findShutdownPods returns the shutdown pods that exist for a device.
//
// Looking them up by label rather than by the name in status is what makes a
// crash between creating the pod and writing that name harmless: the pod is
// found and adopted instead of a second one being created on top of it.
func findShutdownPods(ctx context.Context, c client.Reader, device *v1alpha1.SandmanDevice, namespace string) ([]corev1.Pod, error) {
	var pods corev1.PodList
	err := c.List(ctx, &pods,
		client.InNamespace(namespace),
		client.MatchingLabels{v1alpha1.LabelDevice: device.Name})
	if err != nil {
		return nil, fmt.Errorf("list shutdown pods: %w", err)
	}

	active := make([]corev1.Pod, 0, len(pods.Items))
	for i := range pods.Items {
		if pods.Items[i].DeletionTimestamp == nil {
			active = append(active, pods.Items[i])
		}
	}
	return active, nil
}

// deleteShutdownPods removes a device's shutdown pods.
//
// They are deleted with grace period zero because by then the machine they run
// on is off: the kubelet is not there to acknowledge the delete, and a pod
// waiting for an acknowledgement that cannot come would sit Terminating until
// the node object is removed.
func deleteShutdownPods(ctx context.Context, c client.Client, device *v1alpha1.SandmanDevice, namespace string) error {
	pods, err := findShutdownPods(ctx, c, device, namespace)
	if err != nil {
		return err
	}
	for i := range pods {
		if err := c.Delete(ctx, &pods[i], client.GracePeriodSeconds(0)); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete shutdown pod %s/%s: %w", pods[i].Namespace, pods[i].Name, err)
		}
	}
	return nil
}

// shutdownPodFailure returns a message when the shutdown pod finished without
// the machine going down, and the empty string while it is still plausible
// that the poweroff is in progress.
//
// A poweroff that works usually kills the pod's node before the pod can report
// anything, so a *reported* exit is itself the suspicious case: it means the
// command returned and the machine stayed up.
func shutdownPodFailure(pod *corev1.Pod) string {
	switch pod.Status.Phase {
	case corev1.PodFailed:
		return fmt.Sprintf("shutdown pod %s failed: %s", pod.Name, containerMessage(pod))
	case corev1.PodSucceeded:
		return fmt.Sprintf("shutdown pod %s exited 0 but the node is still up: %s", pod.Name, containerMessage(pod))
	default:
		return ""
	}
}

func containerMessage(pod *corev1.Pod) string {
	if pod.Status.Message != "" {
		return pod.Status.Message
	}
	for i := range pod.Status.ContainerStatuses {
		state := pod.Status.ContainerStatuses[i].State.Terminated
		if state == nil {
			continue
		}
		message := strings.TrimSpace(state.Message)
		if message == "" {
			message = state.Reason
		}
		return fmt.Sprintf("exit code %d %s", state.ExitCode, message)
	}
	return "no container status reported"
}

// resourceQuantity parses a quantity that is known good at compile time. The
// only inputs are the constants above, so a parse failure is a bug rather than
// a runtime condition.
func resourceQuantity(s string) resource.Quantity {
	return resource.MustParse(s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
