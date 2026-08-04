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

package vmservice

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	infrav1 "github.com/ionos-cloud/cluster-api-provider-proxmox/api/v1alpha2"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/proxmox"
)

// TestDeleteVM_RoutedToZoneClient is the anti-orphan proof: deleting a
// machine placed in a credentialed zone must talk to the zone endpoint and
// never to the cluster one, where a same-VMID lookup could answer for an
// unrelated VM.
func TestDeleteVM_RoutedToZoneClient(t *testing.T) {
	machineScope, _, zoneClient, _ := setupZonedReconcilerTest(t, "zone-b")
	require.NoError(t, machineScope.ClientError())

	vm := newRunningVM()
	machineScope.ProxmoxMachine.Spec.VirtualMachineID = new(int64(vm.VMID))
	machineScope.InfraCluster.ProxmoxCluster.AddNodeLocation(infrav1.NodeLocation{
		Machine: corev1.LocalObjectReference{Name: machineScope.Name()},
		Node:    "node1",
	}, false)

	// the cluster mock has no expectations: any call on it fails the test.
	zoneClient.EXPECT().DeleteVM(context.TODO(), "node1", int64(123)).Return(nil, errors.New("vm does not exist: some reason")).Once()

	require.NoError(t, DeleteVM(context.TODO(), machineScope))
	require.Empty(t, machineScope.ProxmoxMachine.Finalizers)
}

// TestFindVM_RoutedToZoneClient proves lookups run against the zone endpoint.
func TestFindVM_RoutedToZoneClient(t *testing.T) {
	machineScope, _, zoneClient, _ := setupZonedReconcilerTest(t, "zone-b")

	vm := newRunningVM()
	machineScope.ProxmoxMachine.Spec.VirtualMachineID = new(int64(vm.VMID))
	machineScope.ProxmoxMachine.Status.ProxmoxNode = new("node1")

	zoneClient.EXPECT().GetVM(context.TODO(), "node1", int64(123)).Return(vm, nil).Once()

	found, err := FindVM(context.TODO(), machineScope)
	require.NoError(t, err)
	require.Equal(t, vm, found)
}

// TestZoneClientError_FailClosed: a machine whose zone disappeared from the
// cluster spec must resolve no client at all — never the cluster one.
func TestZoneClientError_FailClosed(t *testing.T) {
	machineScope, _, _ := setupReconcilerTest(t, func(machine *clusterv1.Machine, _ *infrav1.ProxmoxCluster, _ *infrav1.ProxmoxMachine) {
		machine.Spec.FailureDomain = "zone-gone"
	})

	require.Error(t, machineScope.ClientError())
	require.ErrorContains(t, machineScope.ClientError(), `zone "zone-gone" not configured`)
	require.Nil(t, machineScope.ProxmoxClient())
}

// TestCreateVM_ZoneTemplateOverride proves the zone's templateSource wins
// over the machine's own template fields and that the clone runs on the zone
// client.
func TestCreateVM_ZoneTemplateOverride(t *testing.T) {
	ctx := context.Background()

	machineScope, _, zoneClient, _ := setupZonedReconcilerTest(t, "zone-b",
		func(_ *clusterv1.Machine, infraCluster *infrav1.ProxmoxCluster, _ *infrav1.ProxmoxMachine) {
			infraCluster.Spec.ZoneConfigs[0].TemplateSource = &infrav1.TemplateSource{
				SourceNode: ptr.To("dc-b-node"),
				TemplateID: ptr.To(int32(999)),
			}
		})

	// the machine's own fields (node1/123) must be ignored entirely.
	require.Equal(t, "dc-b-node", machineScope.SourceNode())
	require.EqualValues(t, 999, machineScope.TemplateID())

	expectedOptions := proxmox.VMCloneRequest{
		Node:   "dc-b-node",
		Name:   "test",
		Full:   true,
		Target: "node1",
	}

	response := proxmox.VMCloneResponse{NewID: 456, Task: newTask()}
	zoneClient.EXPECT().GetReservableMemoryBytes(ctx, "node1", int64(100)).Return(^uint64(0), nil).Once()
	zoneClient.EXPECT().CloneVM(ctx, 999, expectedOptions).Return(response, nil).Once()

	_, err := createVM(ctx, machineScope)
	require.NoError(t, err)
	require.Equal(t, "node1", *machineScope.ProxmoxMachine.Status.ProxmoxNode)
}

// TestZoneWithoutCredentials_UsesClusterClient: zones without their own
// credentialsRef keep using the cluster client — the bit-identical guarantee
// for single-endpoint setups.
func TestZoneWithoutCredentials_UsesClusterClient(t *testing.T) {
	machineScope, clusterClient, _ := setupReconcilerTest(t, func(machine *clusterv1.Machine, infraCluster *infrav1.ProxmoxCluster, _ *infrav1.ProxmoxMachine) {
		infraCluster.Spec.ZoneConfigs = []infrav1.ZoneConfigSpec{{
			Zone:       ptr.To("zone-plain"),
			DNSServers: []string{"1.2.3.4"},
			Nodes:      []string{"node1"},
		}}
		machine.Spec.FailureDomain = "zone-plain"
	})

	require.NoError(t, machineScope.ClientError())
	require.Same(t, clusterClient, machineScope.ProxmoxClient())
}
