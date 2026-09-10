package power

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNodeObservationUpAndDown(t *testing.T) {
	grace := 2 * time.Minute

	tests := []struct {
		name      string
		obs       nodeObservation
		wantUp    bool
		wantDown  bool
		reasonWhy string
	}{
		{
			name:      "no node at all",
			obs:       nodeObservation{},
			wantDown:  true,
			reasonWhy: "a machine that never registered or was garbage collected is off",
		},
		{
			name:      "ready node",
			obs:       nodeObservation{found: true, ready: true},
			wantUp:    true,
			reasonWhy: "Ready is the definition of up",
		},
		{
			name:      "not ready but heartbeating",
			obs:       nodeObservation{found: true, heartbeat: testNow.Add(-10 * time.Second)},
			reasonWhy: "a kubelet still reporting means the machine still has power",
		},
		{
			name:      "heartbeat older than the grace period",
			obs:       nodeObservation{found: true, heartbeat: testNow.Add(-5 * time.Minute)},
			wantDown:  true,
			reasonWhy: "nothing else stops a kubelet mid-heartbeat",
		},
		{
			name:      "no heartbeat recorded",
			obs:       nodeObservation{found: true},
			reasonWhy: "without a heartbeat there is no evidence either way, so nothing is concluded",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.obs.up(); got != tc.wantUp {
				t.Fatalf("up() = %v, want %v: %s", got, tc.wantUp, tc.reasonWhy)
			}
			if got := tc.obs.down(testNow, grace); got != tc.wantDown {
				t.Fatalf("down() = %v, want %v: %s", got, tc.wantDown, tc.reasonWhy)
			}
		})
	}
}

func TestNodeAddressPrefersTheInternalOne(t *testing.T) {
	node := &corev1.Node{Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
		{Type: corev1.NodeHostName, Address: "worker-01"},
		{Type: corev1.NodeExternalIP, Address: "203.0.113.10"},
		{Type: corev1.NodeInternalIP, Address: "192.168.1.20"},
	}}}

	if got := nodeAddress(node); got != "192.168.1.20" {
		t.Fatalf("nodeAddress = %q, want the internal address a magic packet can reach", got)
	}
}

func TestNodeAddressFallsBackToExternal(t *testing.T) {
	node := &corev1.Node{Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
		{Type: corev1.NodeHostName, Address: "worker-01"},
		{Type: corev1.NodeExternalIP, Address: "203.0.113.10"},
	}}}

	if got := nodeAddress(node); got != "203.0.113.10" {
		t.Fatalf("nodeAddress = %q, want the external address", got)
	}
}

// A node's Ready condition carries a heartbeat that is refreshed far more often
// than the condition changes, so it is the one to read.
func TestObservationReadsTheHeartbeatFromTheReadyCondition(t *testing.T) {
	beat := metav1.NewTime(testNow.Add(-30 * time.Second))
	node := &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
		{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse, LastHeartbeatTime: metav1.NewTime(testNow)},
		{Type: corev1.NodeReady, Status: corev1.ConditionTrue, LastHeartbeatTime: beat},
	}}}

	var obs nodeObservation
	for i := range node.Status.Conditions {
		cond := &node.Status.Conditions[i]
		if cond.Type == corev1.NodeReady {
			obs = nodeObservation{found: true, ready: cond.Status == corev1.ConditionTrue, heartbeat: cond.LastHeartbeatTime.Time}
		}
	}
	if !obs.heartbeat.Equal(beat.Time) {
		t.Fatalf("heartbeat = %s, want the Ready condition's %s", obs.heartbeat, beat.Time)
	}
}
