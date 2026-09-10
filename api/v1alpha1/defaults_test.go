package v1alpha1

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// The CRD's kubebuilder defaults are applied by the API server. These
// accessors have to agree with them, or a device built in a test behaves
// differently from the same device in a cluster.
func TestWakeOnLANDefaults(t *testing.T) {
	var spec WakeOnLANSpec

	if got := spec.BroadcastAddr(); got != DefaultBroadcastAddress {
		t.Fatalf("broadcast = %q, want %q", got, DefaultBroadcastAddress)
	}
	if got := spec.UDPPort(); got != DefaultPort {
		t.Fatalf("port = %d, want %d", got, DefaultPort)
	}
	if got := spec.PacketCount(); got != DefaultPackets {
		t.Fatalf("packets = %d, want %d", got, DefaultPackets)
	}
	if got := spec.PacketGap(); got != DefaultPacketInterval {
		t.Fatalf("interval = %s, want %s", got, DefaultPacketInterval)
	}

	spec = WakeOnLANSpec{BroadcastAddress: "10.0.0.255", Port: 7, Packets: 1,
		PacketInterval: &metav1.Duration{Duration: time.Second}}
	if spec.BroadcastAddr() != "10.0.0.255" || spec.UDPPort() != 7 || spec.PacketCount() != 1 || spec.PacketGap() != time.Second {
		t.Fatalf("explicit values must win, got %+v", spec)
	}
}

func TestDrainDefaults(t *testing.T) {
	var spec DrainSpec

	if !spec.DrainEnabled() || !spec.CordonEnabled() || !spec.UncordonEnabled() {
		t.Fatal("draining, cordoning and uncordoning are on by default")
	}
	if !spec.IgnoreDaemonSetPods() || !spec.DeleteEmptyDir() {
		t.Fatal("a drain that waits for DaemonSet pods or refuses to discard scratch data never finishes")
	}
	if spec.ForceDelete() {
		t.Fatal("force defaults off: deleting a pod nothing will recreate should be a decision, not a default")
	}
	if got := spec.GracePeriod(); got != DefaultGracePeriodSeconds {
		t.Fatalf("grace period = %d, want %d (each pod keeps its own)", got, DefaultGracePeriodSeconds)
	}
	if got := spec.DrainTimeout(); got != DefaultDrainTimeout {
		t.Fatalf("timeout = %s, want %s", got, DefaultDrainTimeout)
	}

	spec = DrainSpec{Enabled: ptr.To(false), Force: ptr.To(true), GracePeriodSeconds: 30}
	if spec.DrainEnabled() || !spec.ForceDelete() || spec.GracePeriod() != 30 {
		t.Fatalf("explicit values must win, got %+v", spec)
	}
}

func TestShutdownDefaults(t *testing.T) {
	var spec ShutdownSpec

	if got := spec.ShutdownMode(); got != ShutdownModePrivilegedPod {
		t.Fatalf("mode = %q, want %q", got, ShutdownModePrivilegedPod)
	}
	if got := spec.ContainerImage(); got != DefaultShutdownImage {
		t.Fatalf("image = %q", got)
	}
	if got := spec.PodNamespace("sandman"); got != "sandman" {
		t.Fatalf("namespace = %q, want the fallback", got)
	}
	if got := spec.Deadline(); got != DefaultActiveDeadline {
		t.Fatalf("deadline = %d", got)
	}

	command := spec.ShutdownCommand()
	if len(command) == 0 || command[0] != "nsenter" {
		t.Fatalf("command = %v, want one that enters the host's namespaces", command)
	}
	if len(spec.PodTolerations()) == 0 {
		t.Fatal("the shutdown pod has to tolerate the taints of a node that is being drained")
	}
}

func TestTimeoutDefaults(t *testing.T) {
	var spec TimeoutsSpec

	if spec.StartTimeout() != DefaultStartTimeout ||
		spec.StopTimeout() != DefaultStopTimeout ||
		spec.WakeRetry() != DefaultWakeRetryInterval ||
		spec.StoppedGrace() != DefaultStoppedGracePeriod {
		t.Fatalf("timeout defaults = %+v", spec)
	}

	// A zero duration is treated as unset rather than as "immediately", which
	// would fail every transition the moment it began.
	spec = TimeoutsSpec{Start: &metav1.Duration{}}
	if spec.StartTimeout() != DefaultStartTimeout {
		t.Fatalf("a zero start timeout = %s, want the default", spec.StartTimeout())
	}
}

func TestSecretKeyReferenceDefaults(t *testing.T) {
	var ref *SecretKeyReference

	if got := ref.SecretKey(); got != DefaultSecretKey {
		t.Fatalf("key on a nil reference = %q, want %q", got, DefaultSecretKey)
	}
	if got := ref.SecretNamespace("sandman"); got != "sandman" {
		t.Fatalf("namespace on a nil reference = %q", got)
	}

	ref = &SecretKeyReference{Name: "wol", Namespace: "infra", Key: "secure-on"}
	if ref.SecretKey() != "secure-on" || ref.SecretNamespace("sandman") != "infra" {
		t.Fatalf("explicit values must win, got %+v", ref)
	}
}

func TestDesiredDefaultsToUnmanaged(t *testing.T) {
	var spec SandmanDeviceSpec

	if got := spec.Desired(); got != PowerStateUnmanaged {
		t.Fatalf("desired = %q, want Unmanaged: a device nobody has asked anything of is not touched", got)
	}
}

func TestInFlightPhases(t *testing.T) {
	inFlight := map[PowerPhase]bool{
		PowerPhaseUnknown:  false,
		PowerPhasePending:  true,
		PowerPhaseStarting: true,
		PowerPhaseRunning:  false,
		PowerPhaseStopping: true,
		PowerPhaseStopped:  false,
		PowerPhaseFailed:   false,
	}
	for phase, want := range inFlight {
		if got := phase.InFlight(); got != want {
			t.Fatalf("%q.InFlight() = %v, want %v", phase, got, want)
		}
	}
}
