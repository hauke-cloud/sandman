/*
Copyright 2026 hauke.cloud.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The CRD carries the same defaults as kubebuilder markers, but those are
// applied by the API server. These accessors repeat them so that a device read
// from a cache written by an older CRD -- or built in a test -- still behaves,
// and so that no caller has to nil-check its way through the spec.
const (
	DefaultBroadcastAddress   = "255.255.255.255"
	DefaultPort               = 9
	DefaultPackets            = 3
	DefaultPacketInterval     = 200 * time.Millisecond
	DefaultShutdownImage      = "busybox:1.37"
	DefaultActiveDeadline     = 120
	DefaultDrainTimeout       = 5 * time.Minute
	DefaultStartTimeout       = 10 * time.Minute
	DefaultStopTimeout        = 10 * time.Minute
	DefaultWakeRetryInterval  = time.Minute
	DefaultStoppedGracePeriod = 2 * time.Minute
	DefaultSecretKey          = "password"
	DefaultGracePeriodSeconds = int64(-1)
)

// DefaultShutdownCommand powers the host off from inside a container.
//
// nsenter enters PID 1's namespaces -- mount, UTS, IPC, network and PID -- so
// systemctl talks to the host's systemd over its own socket rather than
// looking for one in the container. `poweroff` on its own would signal the
// container's init, which the kernel ignores.
func DefaultShutdownCommand() []string {
	return []string{
		"nsenter", "-t", "1", "-m", "-u", "-i", "-n", "-p", "--",
		"systemctl", "poweroff",
	}
}

// DefaultTolerations let the shutdown pod land on the node it has to stop.
// That node is cordoned by then, and may already be reporting NotReady or
// unreachable, so a pod with the usual tolerations would never be admitted.
func DefaultTolerations() []corev1.Toleration {
	return []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
}

// BroadcastAddr returns the configured broadcast address or the default.
func (w *WakeOnLANSpec) BroadcastAddr() string {
	if w.BroadcastAddress == "" {
		return DefaultBroadcastAddress
	}
	return w.BroadcastAddress
}

// UDPPort returns the configured port or the default.
func (w *WakeOnLANSpec) UDPPort() int {
	if w.Port <= 0 {
		return DefaultPort
	}
	return int(w.Port)
}

// PacketCount returns how many magic packets to send per attempt.
func (w *WakeOnLANSpec) PacketCount() int {
	if w.Packets <= 0 {
		return DefaultPackets
	}
	return int(w.Packets)
}

// PacketGap returns the delay between magic packets.
func (w *WakeOnLANSpec) PacketGap() time.Duration {
	return durationOr(w.PacketInterval, DefaultPacketInterval)
}

// SecretKey returns the key holding the SecureOn password.
func (r *SecretKeyReference) SecretKey() string {
	if r == nil || r.Key == "" {
		return DefaultSecretKey
	}
	return r.Key
}

// SecretNamespace returns the namespace of the referenced Secret, falling back
// to the namespace sandman itself runs in.
func (r *SecretKeyReference) SecretNamespace(fallback string) string {
	if r == nil || r.Namespace == "" {
		return fallback
	}
	return r.Namespace
}

// ShutdownMode returns the effective shutdown mode.
func (s *ShutdownSpec) ShutdownMode() ShutdownMode {
	if s.Mode == "" {
		return ShutdownModePrivilegedPod
	}
	return s.Mode
}

// ContainerImage returns the shutdown pod's image.
func (s *ShutdownSpec) ContainerImage() string {
	if s.Image == "" {
		return DefaultShutdownImage
	}
	return s.Image
}

// ShutdownCommand returns the command the shutdown pod runs.
func (s *ShutdownSpec) ShutdownCommand() []string {
	if len(s.Command) > 0 {
		return s.Command
	}
	return DefaultShutdownCommand()
}

// PodNamespace returns where the shutdown pod is created, falling back to the
// namespace sandman runs in.
func (s *ShutdownSpec) PodNamespace(fallback string) string {
	if s.Namespace == "" {
		return fallback
	}
	return s.Namespace
}

// PodTolerations returns the shutdown pod's tolerations.
func (s *ShutdownSpec) PodTolerations() []corev1.Toleration {
	if len(s.Tolerations) > 0 {
		return s.Tolerations
	}
	return DefaultTolerations()
}

// Deadline returns the shutdown pod's activeDeadlineSeconds.
func (s *ShutdownSpec) Deadline() int64 {
	if s.ActiveDeadlineSeconds <= 0 {
		return DefaultActiveDeadline
	}
	return s.ActiveDeadlineSeconds
}

// DrainEnabled reports whether pods are evicted before a stop.
func (d *DrainSpec) DrainEnabled() bool { return boolOr(d.Enabled, true) }

// CordonEnabled reports whether the node is cordoned before a stop.
func (d *DrainSpec) CordonEnabled() bool { return boolOr(d.Cordon, true) }

// UncordonEnabled reports whether the node is uncordoned after a start.
func (d *DrainSpec) UncordonEnabled() bool { return boolOr(d.UncordonOnStart, true) }

// IgnoreDaemonSetPods reports whether DaemonSet pods are skipped.
func (d *DrainSpec) IgnoreDaemonSetPods() bool { return boolOr(d.IgnoreDaemonSets, true) }

// DeleteEmptyDir reports whether pods with emptyDir volumes are evicted.
func (d *DrainSpec) DeleteEmptyDir() bool { return boolOr(d.DeleteEmptyDirData, true) }

// ForceDelete reports whether unmanaged pods are deleted outright.
func (d *DrainSpec) ForceDelete() bool { return boolOr(d.Force, false) }

// GracePeriod returns the per-pod grace period override, negative meaning
// each pod keeps its own.
func (d *DrainSpec) GracePeriod() int64 {
	if d.GracePeriodSeconds == 0 {
		return DefaultGracePeriodSeconds
	}
	return d.GracePeriodSeconds
}

// DrainTimeout returns the drain's deadline.
func (d *DrainSpec) DrainTimeout() time.Duration { return durationOr(d.Timeout, DefaultDrainTimeout) }

// StartTimeout returns how long a start may take.
func (t *TimeoutsSpec) StartTimeout() time.Duration { return durationOr(t.Start, DefaultStartTimeout) }

// StopTimeout returns how long a stop may take.
func (t *TimeoutsSpec) StopTimeout() time.Duration { return durationOr(t.Stop, DefaultStopTimeout) }

// WakeRetry returns how often a magic packet is resent during a start.
func (t *TimeoutsSpec) WakeRetry() time.Duration {
	return durationOr(t.WakeRetryInterval, DefaultWakeRetryInterval)
}

// StoppedGrace returns how long an unreachable node is given before it counts
// as stopped.
func (t *TimeoutsSpec) StoppedGrace() time.Duration {
	return durationOr(t.StoppedGracePeriod, DefaultStoppedGracePeriod)
}

// Desired returns the effective desired power state.
func (s *SandmanDeviceSpec) Desired() PowerState {
	if s.DesiredPowerState == "" {
		return PowerStateUnmanaged
	}
	return s.DesiredPowerState
}

func boolOr(p *bool, fallback bool) bool {
	if p == nil {
		return fallback
	}
	return *p
}

func durationOr(d *metav1.Duration, fallback time.Duration) time.Duration {
	if d == nil || d.Duration <= 0 {
		return fallback
	}
	return d.Duration
}
