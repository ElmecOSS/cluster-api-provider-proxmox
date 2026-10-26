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
	"strings"

	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1 "github.com/ionos-cloud/cluster-api-provider-proxmox/api/v1alpha2"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/proxmox/goproxmox"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/scope"
)

// DeleteVM implements the logic of destroying a VM.
func DeleteVM(ctx context.Context, machineScope *scope.MachineScope) error {
	vmID := machineScope.ProxmoxMachine.GetVirtualMachineID()
	node := machineScope.LocateProxmoxNode()

	_, err := machineScope.ProxmoxClient().DeleteVM(ctx, node, vmID)
	if err == nil {
		return nil
	}

	if VMNotFound(err) || errors.Is(err, goproxmox.ErrVMIDFree) {
		// remove machine from cluster status
		machineScope.InfraCluster.ProxmoxCluster.RemoveNodeLocation(machineScope.Name(), util.IsControlPlaneMachine(machineScope.Machine))
		// The VM is deleted so remove the finalizer.
		ctrlutil.RemoveFinalizer(machineScope.ProxmoxMachine, infrav1.MachineFinalizer)
		return machineScope.InfraCluster.PatchObject()
	}

	if errors.Is(err, goproxmox.ErrVMNotOnNode) {
		if relocated := relocateForDeletion(ctx, machineScope, vmID, node); relocated != nil {
			err = relocated
		}
	}

	conditions.Set(machineScope.ProxmoxMachine, metav1.Condition{
		Type:   infrav1.ProxmoxMachineVirtualMachineProvisionedCondition,
		Status: metav1.ConditionFalse,
		Reason: infrav1.ProxmoxMachineVirtualMachineProvisionedDeletionFailedReason,
	})

	return err
}

// relocateForDeletion points the machine at the node that actually holds the
// VM, so the next deletion attempt asks that node instead of the recorded one.
//
// Proxmox answers for a config file that is not on the node asked the same way
// whether the VM moved away or is still being cloned onto another node, so the
// cluster-wide lookup is what separates the two: a VM found elsewhere was
// migrated and the recorded node is merely stale. This matters only here
// because deletion calls DeleteVM directly and never passes through
// ReconcileVM, where updateVMLocation would have corrected the node already --
// left alone, every retry would keep asking the same wrong node.
//
// It returns an error describing the move when it relocated the machine, and
// nil when there was nothing to correct, leaving the original error to stand.
func relocateForDeletion(ctx context.Context, machineScope *scope.MachineScope, vmID int64, staleNode string) error {
	rsc, err := machineScope.ProxmoxClient().FindVMResource(ctx, uint64(vmID))
	if err != nil {
		// Not findable anywhere. A clone in flight lands here too, so this is
		// not an error worth surfacing: leave the caller's error to stand.
		//nolint:nilerr // returning nil deliberately: there is nothing to correct.
		return nil
	}

	if rsc == nil || rsc.Node == "" || rsc.Node == staleNode {
		return nil
	}

	// Only follow the VMID when it still resolves to this machine's VM. A
	// recycled VMID would otherwise point deletion at somebody else's VM, and
	// DeleteVM deletes by id without checking the name. Refusing to retarget
	// leaves the machine stuck, which is the safe half of that choice.
	if rsc.Name != machineScope.Name() {
		return errors.Errorf("VM %d on node %s is named %q, not %q: refusing to delete it",
			vmID, rsc.Node, rsc.Name, machineScope.Name())
	}

	machineScope.ProxmoxMachine.Status.ProxmoxNode = new(rsc.Node)
	machineScope.InfraCluster.ProxmoxCluster.UpdateNodeLocation(
		machineScope.Name(),
		rsc.Node,
		machineScope.Zone(),
		util.IsControlPlaneMachine(machineScope.Machine),
	)

	if err := machineScope.InfraCluster.PatchObject(); err != nil {
		return errors.Wrapf(err, "failed to record that VM %d moved to node %s", vmID, rsc.Node)
	}

	return errors.Errorf("VM %d is on node %s, not %s: retrying deletion there", vmID, rsc.Node, staleNode)
}

// VMNotFound checks if the given err is related to that the VM is not found in Proxmox.
//
// The test is a substring match because Proxmox reports a missing VM as a
// missing config file rather than a typed error. That phrasing is also what it
// returns for a VM that exists but whose config is not on the node asked --
// during a clone onto a node other than the template's, for instance -- so
// ErrVMNotOnNode is excluded explicitly: acting on it would drop the finalizer
// of a VM that is about to exist.
func VMNotFound(err error) bool {
	if errors.Is(err, goproxmox.ErrVMNotOnNode) {
		return false
	}

	return strings.Contains(err.Error(), "does not exist")
}
