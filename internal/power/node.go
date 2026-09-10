// Package power drives machines between on and off, and reports where they
// are. It is the only part of sandman that touches hardware or the cluster:
// the REST API writes a desired state and this reconciles towards it.
package power

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// nodeObservation is everything the reconciler needs to know about a node,
// flattened out of the Node object so the state machine reads as a state
// machine rather than as a walk over conditions.
type nodeObservation struct {
	// found reports whether a Node object exists at all. A machine that has
	// been off long enough for its node to be deleted has none, and one that
	// never joined the cluster never had one.
	found bool
	// ready mirrors the Ready condition being true.
	ready bool
	// unschedulable mirrors the cordon.
	unschedulable bool
	// address is the node's internal address, recorded while it is up.
	address string
	// heartbeat is when the kubelet last reported. When a machine loses power
	// its Ready condition does not go false -- the kubelet is not there to
	// say so -- it simply stops being refreshed, so the age of this is the
	// only real evidence that the machine is gone.
	heartbeat time.Time
	// node is the object itself, for cordon patches.
	node *corev1.Node
}

// observeNode reads the current state of a device's node.
func observeNode(ctx context.Context, c client.Reader, name string) (nodeObservation, error) {
	var node corev1.Node
	if err := c.Get(ctx, types.NamespacedName{Name: name}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return nodeObservation{}, nil
		}
		return nodeObservation{}, err
	}

	obs := nodeObservation{
		found:         true,
		unschedulable: node.Spec.Unschedulable,
		node:          &node,
	}
	for i := range node.Status.Conditions {
		cond := &node.Status.Conditions[i]
		if cond.Type != corev1.NodeReady {
			continue
		}
		obs.ready = cond.Status == corev1.ConditionTrue
		obs.heartbeat = cond.LastHeartbeatTime.Time
		if obs.heartbeat.IsZero() {
			obs.heartbeat = cond.LastTransitionTime.Time
		}
		break
	}
	obs.address = nodeAddress(&node)
	return obs, nil
}

// up reports whether the machine is running and usable.
func (o nodeObservation) up() bool { return o.found && o.ready }

// down reports whether the machine can be considered off.
//
// A missing node is the clean case. The messy one is a node object that
// outlives its machine: the kubelet stopped mid-heartbeat, so the node sits
// there with a stale Ready condition until the node controller marks it
// Unknown. Waiting out a grace period on the heartbeat covers both, and covers
// a machine whose node object is never garbage collected at all.
func (o nodeObservation) down(now time.Time, grace time.Duration) bool {
	if !o.found {
		return true
	}
	if o.ready {
		return false
	}
	if o.heartbeat.IsZero() {
		return false
	}
	return now.Sub(o.heartbeat) > grace
}

// nodeAddress prefers the internal address, which is the one on the network a
// magic packet has to reach.
func nodeAddress(node *corev1.Node) string {
	var fallback string
	for _, addr := range node.Status.Addresses {
		switch addr.Type {
		case corev1.NodeInternalIP:
			return addr.Address
		case corev1.NodeExternalIP:
			if fallback == "" {
				fallback = addr.Address
			}
		}
	}
	return fallback
}

// setSchedulable cordons or uncordons a node. It is a no-op when the node is
// already in the wanted state, so it can be called on every reconcile.
func setSchedulable(ctx context.Context, c client.Client, node *corev1.Node, schedulable bool) (bool, error) {
	if node == nil {
		return false, nil
	}
	wantUnschedulable := !schedulable
	if node.Spec.Unschedulable == wantUnschedulable {
		return false, nil
	}

	patch := client.MergeFrom(node.DeepCopy())
	node.Spec.Unschedulable = wantUnschedulable
	if err := c.Patch(ctx, node, patch); err != nil {
		return false, err
	}
	return true, nil
}
