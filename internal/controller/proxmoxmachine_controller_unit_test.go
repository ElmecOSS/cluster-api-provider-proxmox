/*
Copyright 2023-2026 IONOS Cloud.

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

package controller

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1 "github.com/ionos-cloud/cluster-api-provider-proxmox/api/v1alpha2"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/kubernetes/ipam"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/proxmox/proxmoxtest"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/scope"
)

// newDeleteTestMachineScope builds a MachineScope for reconcileDelete tests.
// The machine references the given failure domain; a zone that is not
// configured in the ProxmoxCluster leaves the scope with a ClientError.
func newDeleteTestMachineScope(t *testing.T, failureDomain string, vmID *int64) (*ProxmoxMachineReconciler, *scope.MachineScope) {
	t.Helper()

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: metav1.NamespaceDefault},
	}
	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: metav1.NamespaceDefault,
			Labels:    map[string]string{clusterv1.ClusterNameLabel: "test"},
		},
	}
	machine.Spec.FailureDomain = failureDomain

	infraCluster := &infrav1.ProxmoxCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: metav1.NamespaceDefault},
		Spec: infrav1.ProxmoxClusterSpec{
			IPv4Config: &infrav1.IPConfigSpec{
				Addresses: []string{"10.0.0.10-10.0.0.20"},
				Prefix:    24,
				Gateway:   "10.0.0.1",
			},
			DNSServers: []string{"1.2.3.4"},
		},
		Status: infrav1.ProxmoxClusterStatus{NodeLocations: &infrav1.NodeLocations{}},
	}

	infraMachine := &infrav1.ProxmoxMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "test",
			Namespace:  metav1.NamespaceDefault,
			Finalizers: []string{infrav1.MachineFinalizer},
			Labels:     map[string]string{clusterv1.ClusterNameLabel: "test"},
		},
		Spec: infrav1.ProxmoxMachineSpec{
			VirtualMachineID: vmID,
			VirtualMachineCloneSpec: infrav1.VirtualMachineCloneSpec{
				TemplateSource: infrav1.TemplateSource{
					SourceNode: ptr.To("node1"),
					TemplateID: ptr.To(int32(123)),
				},
			},
		},
	}

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, clusterv1.AddToScheme(scheme))
	require.NoError(t, infrav1.AddToScheme(scheme))
	kubeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cluster, machine, infraCluster, infraMachine).
		WithStatusSubresource(&infrav1.ProxmoxCluster{}, &infrav1.ProxmoxMachine{}).
		Build()

	logger := logr.Discard()
	clusterScope, err := scope.NewClusterScope(scope.ClusterScopeParams{
		Client:         kubeClient,
		Logger:         &logger,
		Cluster:        cluster,
		ProxmoxCluster: infraCluster,
		ProxmoxClient:  proxmoxtest.NewMockClient(t),
		IPAMHelper:     &ipam.Helper{},
	})
	require.NoError(t, err)

	machineScope, err := scope.NewMachineScope(scope.MachineScopeParams{
		Client:         kubeClient,
		Logger:         &logger,
		Cluster:        cluster,
		Machine:        machine,
		InfraCluster:   clusterScope,
		ProxmoxMachine: infraMachine,
		IPAMHelper:     &ipam.Helper{},
	})
	require.NoError(t, err)

	return &ProxmoxMachineReconciler{Client: kubeClient}, machineScope
}

// TestReconcileDelete_ZoneClientUnavailable_KeepsFinalizer is the anti-orphan
// guarantee at the controller level: a machine whose zone client cannot be
// resolved must never lose its finalizer, or its VM would be orphaned (or a
// wrong-endpoint VM destroyed).
func TestReconcileDelete_ZoneClientUnavailable_KeepsFinalizer(t *testing.T) {
	r, machineScope := newDeleteTestMachineScope(t, "zone-gone", ptr.To(int64(123)))
	require.Error(t, machineScope.ClientError())

	_, err := r.reconcileDelete(context.Background(), machineScope)
	require.ErrorContains(t, err, "zone client unavailable")
	require.True(t, ctrlutil.ContainsFinalizer(machineScope.ProxmoxMachine, infrav1.MachineFinalizer),
		"finalizer must be retained while the zone client is unresolvable")
}

// TestReconcileDelete_NeverCreated_RemovesFinalizer: machines that never got
// a VMID nor an in-flight task have nothing to clean up on Proxmox and must
// be deletable even when their zone is misconfigured.
func TestReconcileDelete_NeverCreated_RemovesFinalizer(t *testing.T) {
	r, machineScope := newDeleteTestMachineScope(t, "zone-gone", nil)
	require.Error(t, machineScope.ClientError())
	require.Nil(t, machineScope.ProxmoxMachine.Status.TaskRef)

	_, err := r.reconcileDelete(context.Background(), machineScope)
	require.NoError(t, err)
	require.False(t, ctrlutil.ContainsFinalizer(machineScope.ProxmoxMachine, infrav1.MachineFinalizer),
		"a machine that never reached Proxmox must be deletable without a zone client")
}
