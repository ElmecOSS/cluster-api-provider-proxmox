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
	"fmt"
	"testing"

	"github.com/luthermonson/go-proxmox"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"

	infrav1 "github.com/ionos-cloud/cluster-api-provider-proxmox/api/v1alpha2"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/proxmox/goproxmox"
)

func TestDeleteVM_SuccessNotFound(t *testing.T) {
	machineScope, proxmoxClient, _ := setupReconcilerTest(t)
	vm := newRunningVM()
	machineScope.ProxmoxMachine.Spec.VirtualMachineID = new(int64(vm.VMID))
	machineScope.InfraCluster.ProxmoxCluster.AddNodeLocation(infrav1.NodeLocation{
		Machine: corev1.LocalObjectReference{Name: machineScope.Name()},
		Node:    "node1",
	}, false)

	proxmoxClient.EXPECT().DeleteVM(context.TODO(), "node1", int64(123)).Return(nil, errors.New("vm does not exist: some reason")).Once()

	require.NoError(t, DeleteVM(context.TODO(), machineScope))
	require.Empty(t, machineScope.ProxmoxMachine.Finalizers)
	require.Empty(t, machineScope.InfraCluster.ProxmoxCluster.GetNode(machineScope.Name(), false))
}

// A VM still being cloned onto its destination node reports as missing there,
// in the same words Proxmox uses for a VM that was never created. Dropping the
// finalizer on that answer leaves the clone to finish into a VM nothing owns,
// so the error has to come back and the machine has to stay registered.
func TestDeleteVM_MidCloneKeepsTheFinalizer(t *testing.T) {
	machineScope, proxmoxClient, _ := setupReconcilerTest(t)
	vm := newRunningVM()
	machineScope.ProxmoxMachine.Spec.VirtualMachineID = new(int64(vm.VMID))
	machineScope.InfraCluster.ProxmoxCluster.AddNodeLocation(infrav1.NodeLocation{
		Machine: corev1.LocalObjectReference{Name: machineScope.Name()},
		Node:    "node1",
	}, false)

	midClone := fmt.Errorf("%w: id 123 on node node1: %w", goproxmox.ErrVMNotOnNode,
		errors.New("Configuration file 'nodes/node1/qemu-server/123.conf' does not exist"))
	proxmoxClient.EXPECT().DeleteVM(context.TODO(), "node1", int64(123)).Return(nil, midClone).Once()
	// A clone in flight is not registered on any other node, so the relocation
	// lookup finds nothing and the original error has to survive untouched.
	proxmoxClient.EXPECT().FindVMResource(context.TODO(), uint64(123)).
		Return(nil, errors.New("unable to find VM with ID 123 on any of the nodes")).Once()

	require.ErrorIs(t, DeleteVM(context.TODO(), machineScope), goproxmox.ErrVMNotOnNode)
	require.NotEmpty(t, machineScope.ProxmoxMachine.Finalizers, "the finalizer must survive a VM that still exists")
	require.NotEmpty(t, machineScope.InfraCluster.ProxmoxCluster.GetNode(machineScope.Name(), false))
}

// A VM migrated to another node answers on the node we recorded exactly as a
// clone in flight does. Telling them apart needs the cluster-wide lookup: once
// the VM turns up elsewhere the recorded node is simply stale, and deletion
// has to be retargeted or every retry asks the same wrong node forever.
func TestDeleteVM_MigratedVMRetargetsTheNode(t *testing.T) {
	machineScope, proxmoxClient, _ := setupReconcilerTest(t)
	vm := newRunningVM()
	machineScope.ProxmoxMachine.Spec.VirtualMachineID = new(int64(vm.VMID))
	machineScope.ProxmoxMachine.Status.ProxmoxNode = new("node1")
	machineScope.InfraCluster.ProxmoxCluster.AddNodeLocation(infrav1.NodeLocation{
		Machine: corev1.LocalObjectReference{Name: machineScope.Name()},
		Node:    "node1",
	}, false)

	migrated := fmt.Errorf("%w: id 123 on node node1: %w", goproxmox.ErrVMNotOnNode,
		errors.New("Configuration file 'nodes/node1/qemu-server/123.conf' does not exist"))
	proxmoxClient.EXPECT().DeleteVM(context.TODO(), "node1", int64(123)).Return(nil, migrated).Once()
	proxmoxClient.EXPECT().FindVMResource(context.TODO(), uint64(123)).
		Return(&proxmox.ClusterResource{VMID: 123, Node: "node2", Name: machineScope.Name()}, nil).Once()

	err := DeleteVM(context.TODO(), machineScope)
	require.ErrorContains(t, err, "node2")
	require.NotEmpty(t, machineScope.ProxmoxMachine.Finalizers, "a VM that still exists keeps its finalizer")
	require.Equal(t, "node2", *machineScope.ProxmoxMachine.Status.ProxmoxNode)
	require.Equal(t, "node2", machineScope.InfraCluster.ProxmoxCluster.GetNode(machineScope.Name(), false))
}

// Retargeting must not drop the machine's zone from its node location: the
// scheduler reads that field, and an entry that loses it stops counting
// towards its zone.
func TestDeleteVM_MigratedVMKeepsTheZone(t *testing.T) {
	machineScope, zoneClient := setupZonedReconcilerTest(t, "zone-b")
	vm := newRunningVM()
	machineScope.ProxmoxMachine.Spec.VirtualMachineID = new(int64(vm.VMID))
	machineScope.InfraCluster.ProxmoxCluster.AddNodeLocation(infrav1.NodeLocation{
		Machine: corev1.LocalObjectReference{Name: machineScope.Name()},
		Node:    "node1",
		Zone:    new("zone-b"),
	}, false)

	migrated := fmt.Errorf("%w: id 123 on node node1: %w", goproxmox.ErrVMNotOnNode,
		errors.New("Configuration file 'nodes/node1/qemu-server/123.conf' does not exist"))
	zoneClient.EXPECT().DeleteVM(context.TODO(), "node1", int64(123)).Return(nil, migrated).Once()
	zoneClient.EXPECT().FindVMResource(context.TODO(), uint64(123)).
		Return(&proxmox.ClusterResource{VMID: 123, Node: "node2", Name: machineScope.Name()}, nil).Once()

	require.Error(t, DeleteVM(context.TODO(), machineScope))

	locations := machineScope.InfraCluster.ProxmoxCluster.Status.NodeLocations.Workers
	require.Len(t, locations, 1)
	require.Equal(t, "node2", locations[0].Node)
	require.NotNil(t, locations[0].Zone, "the zone must survive the retarget")
	require.Equal(t, "zone-b", *locations[0].Zone)
}

// A VMID that was recycled to an unrelated VM must never be followed: DeleteVM
// deletes by id without checking the name, so retargeting here would point the
// next attempt at somebody else's VM. The machine staying stuck is the safe
// half of that trade.
func TestDeleteVM_RecycledVMIDRefusesToFollow(t *testing.T) {
	machineScope, proxmoxClient, _ := setupReconcilerTest(t)
	vm := newRunningVM()
	machineScope.ProxmoxMachine.Spec.VirtualMachineID = new(int64(vm.VMID))
	machineScope.ProxmoxMachine.Status.ProxmoxNode = new("node1")
	machineScope.InfraCluster.ProxmoxCluster.AddNodeLocation(infrav1.NodeLocation{
		Machine: corev1.LocalObjectReference{Name: machineScope.Name()},
		Node:    "node1",
	}, false)

	migrated := fmt.Errorf("%w: id 123 on node node1: %w", goproxmox.ErrVMNotOnNode,
		errors.New("Configuration file 'nodes/node1/qemu-server/123.conf' does not exist"))
	proxmoxClient.EXPECT().DeleteVM(context.TODO(), "node1", int64(123)).Return(nil, migrated).Once()
	proxmoxClient.EXPECT().FindVMResource(context.TODO(), uint64(123)).
		Return(&proxmox.ClusterResource{VMID: 123, Node: "node2", Name: "somebody-else"}, nil).Once()

	err := DeleteVM(context.TODO(), machineScope)
	require.ErrorContains(t, err, "refusing")
	require.NotEmpty(t, machineScope.ProxmoxMachine.Finalizers)
	require.Equal(t, "node1", *machineScope.ProxmoxMachine.Status.ProxmoxNode, "the recorded node must not move")
	require.Equal(t, "node1", machineScope.InfraCluster.ProxmoxCluster.GetNode(machineScope.Name(), false))
}

// A VM that is genuinely gone from the whole cluster must still clean up: the
// relocation lookup finds nothing and the original path runs unchanged.
func TestDeleteVM_GoneEverywhereStillCleansUp(t *testing.T) {
	machineScope, proxmoxClient, _ := setupReconcilerTest(t)
	vm := newRunningVM()
	machineScope.ProxmoxMachine.Spec.VirtualMachineID = new(int64(vm.VMID))
	machineScope.InfraCluster.ProxmoxCluster.AddNodeLocation(infrav1.NodeLocation{
		Machine: corev1.LocalObjectReference{Name: machineScope.Name()},
		Node:    "node1",
	}, false)

	proxmoxClient.EXPECT().DeleteVM(context.TODO(), "node1", int64(123)).
		Return(nil, errors.New("vm does not exist: some reason")).Once()

	require.NoError(t, DeleteVM(context.TODO(), machineScope))
	require.Empty(t, machineScope.ProxmoxMachine.Finalizers)
	require.Empty(t, machineScope.InfraCluster.ProxmoxCluster.GetNode(machineScope.Name(), false))
}

func TestVMNotFound(t *testing.T) {
	require.True(t, VMNotFound(errors.New("vm does not exist: some reason")))
	require.False(t, VMNotFound(errors.New("not authorized to access endpoint")))
	require.False(t, VMNotFound(fmt.Errorf("%w: id 123 on node node1: %w", goproxmox.ErrVMNotOnNode,
		errors.New("Configuration file 'nodes/node1/qemu-server/123.conf' does not exist"))),
		"the wrapped text still says 'does not exist'; the sentinel has to win")
}
