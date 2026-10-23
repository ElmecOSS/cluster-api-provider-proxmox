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

	require.ErrorIs(t, DeleteVM(context.TODO(), machineScope), goproxmox.ErrVMNotOnNode)
	require.NotEmpty(t, machineScope.ProxmoxMachine.Finalizers, "the finalizer must survive a VM that still exists")
	require.NotEmpty(t, machineScope.InfraCluster.ProxmoxCluster.GetNode(machineScope.Name(), false))
}

func TestVMNotFound(t *testing.T) {
	require.True(t, VMNotFound(errors.New("vm does not exist: some reason")))
	require.False(t, VMNotFound(errors.New("not authorized to access endpoint")))
	require.False(t, VMNotFound(fmt.Errorf("%w: id 123 on node node1: %w", goproxmox.ErrVMNotOnNode,
		errors.New("Configuration file 'nodes/node1/qemu-server/123.conf' does not exist"))),
		"the wrapped text still says 'does not exist'; the sentinel has to win")
}
