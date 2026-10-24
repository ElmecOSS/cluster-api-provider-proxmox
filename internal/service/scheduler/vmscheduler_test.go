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

package scheduler

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/ionos-cloud/cluster-api-provider-proxmox/api/v1alpha2"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/kubernetes/ipam"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/proxmox/proxmoxtest"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/scope"
)

type fakeResourceClient struct {
	memory      map[string]uint64
	memoryTotal map[string]uint64
	cpus        map[string]int
	cpusTotal   map[string]int
}

func (c fakeResourceClient) GetReservableMemoryBytes(_ context.Context, nodeName string, _ int64) (uint64, uint64, error) {
	total := c.memoryTotal[nodeName]
	if total == 0 {
		total = c.memory[nodeName] // fallback: treat available as total for legacy tests
	}
	return c.memory[nodeName], total, nil
}

func (c fakeResourceClient) GetReservableCPUCores(_ context.Context, nodeName string, _ int64) (int, int, error) {
	total := c.cpusTotal[nodeName]
	if total == 0 {
		total = c.cpus[nodeName]
	}
	return c.cpus[nodeName], total, nil
}

// errorResourceClient always returns the given error for the specified method.
type errorResourceClient struct {
	fakeResourceClient
	memErr error
	cpuErr error
}

func (c errorResourceClient) GetReservableMemoryBytes(ctx context.Context, nodeName string, adj int64) (uint64, uint64, error) {
	if c.memErr != nil {
		return 0, 0, c.memErr
	}
	return c.fakeResourceClient.GetReservableMemoryBytes(ctx, nodeName, adj)
}

func (c errorResourceClient) GetReservableCPUCores(ctx context.Context, nodeName string, adj int64) (int, int, error) {
	if c.cpuErr != nil {
		return 0, 0, c.cpuErr
	}
	return c.fakeResourceClient.GetReservableCPUCores(ctx, nodeName, adj)
}

func miBytes(in int32) uint64 {
	return uint64(in) * 1024 * 1024
}

func TestSelectNode(t *testing.T) {
	allowedNodes := []string{"pve1", "pve2", "pve3"}
	var locations []infrav1.NodeLocation
	var requestMiB = int32(8)
	availableMem := map[string]uint64{
		"pve1": miBytes(20),
		"pve2": miBytes(30),
		"pve3": miBytes(15),
	}

	expectedNodes := []string{
		// initial pass: pick by memory headroom score
		"pve2", "pve1", "pve3",
		// second pass: pve3 out of memory; pending inflation drives the spread
		"pve2", "pve2", "pve1",
	}

	for i, expectedNode := range expectedNodes {
		t.Run(fmt.Sprintf("round %d", i+1), func(t *testing.T) {
			proxmoxMachine := &infrav1.ProxmoxMachine{
				Spec: infrav1.ProxmoxMachineSpec{
					MemoryMiB: &requestMiB,
				},
			}

			cl := fakeResourceClient{memory: availableMem}

			node, err := selectNode(context.Background(), cl, proxmoxMachine, locations, allowedNodes, nil, &infrav1.SchedulerHints{})
			require.NoError(t, err)
			require.Equal(t, expectedNode, node)

			require.Greater(t, availableMem[node], miBytes(requestMiB))
			availableMem[node] -= miBytes(requestMiB)

			locations = append(locations, infrav1.NodeLocation{Node: node})
		})
	}

	t.Run("out of memory", func(t *testing.T) {
		proxmoxMachine := &infrav1.ProxmoxMachine{
			Spec: infrav1.ProxmoxMachineSpec{
				MemoryMiB: &requestMiB,
			},
		}

		cl := fakeResourceClient{memory: availableMem}

		node, err := selectNode(context.Background(), cl, proxmoxMachine, locations, allowedNodes, nil, &infrav1.SchedulerHints{})
		require.ErrorAs(t, err, &InsufficientMemoryError{})
		require.Empty(t, node)

		expectMem := map[string]uint64{
			"pve1": miBytes(4), // 20 - 8 x 2
			"pve2": miBytes(6), // 30 - 8 x 3
			"pve3": miBytes(7), // 15 - 8 x 1
		}
		require.Equal(t, expectMem, availableMem)
	})
}

func TestSelectNodeEvenlySpread(t *testing.T) {
	// Verify that VMs are scheduled evenly across nodes when memory allows
	allowedNodes := []string{"pve1", "pve2", "pve3"}
	var locations []infrav1.NodeLocation
	var requestMiB = int32(8)
	availableMem := map[string]uint64{
		"pve1": miBytes(25), // enough for 3 VMs
		"pve2": miBytes(35), // enough for 4 VMs
		"pve3": miBytes(15), // enough for 1 VM
	}

	expectedNodes := []string{
		// initial pass: pick by memory headroom score
		"pve2", "pve1", "pve3",
		// second pass: pve3 out of memory; pending inflation spreads across pve1/pve2
		"pve2", "pve1", "pve2",
		// third pass: pve1 and pve2 have room for one more VM each
		"pve2", "pve1",
	}

	for i, expectedNode := range expectedNodes {
		t.Run(fmt.Sprintf("round %d", i+1), func(t *testing.T) {
			proxmoxMachine := &infrav1.ProxmoxMachine{
				Spec: infrav1.ProxmoxMachineSpec{
					MemoryMiB: &requestMiB,
				},
			}

			cl := fakeResourceClient{memory: availableMem}

			node, err := selectNode(context.Background(), cl, proxmoxMachine, locations, allowedNodes, nil, &infrav1.SchedulerHints{})
			require.NoError(t, err)
			require.Equal(t, expectedNode, node)

			require.Greater(t, availableMem[node], miBytes(requestMiB))
			availableMem[node] -= miBytes(requestMiB)

			locations = append(locations, infrav1.NodeLocation{Node: node})
		})
	}

	t.Run("out of memory", func(t *testing.T) {
		proxmoxMachine := &infrav1.ProxmoxMachine{
			Spec: infrav1.ProxmoxMachineSpec{
				MemoryMiB: &requestMiB,
			},
		}

		cl := fakeResourceClient{memory: availableMem}

		node, err := selectNode(context.Background(), cl, proxmoxMachine, locations, allowedNodes, nil, &infrav1.SchedulerHints{})
		require.ErrorAs(t, err, &InsufficientMemoryError{})
		require.Empty(t, node)

		expectMem := map[string]uint64{
			"pve1": miBytes(1), // 25 - 8 x 3
			"pve2": miBytes(3), // 35 - 8 x 4
			"pve3": miBytes(7), // 15 - 8 x 1
		}
		require.Equal(t, expectMem, availableMem)
	})
}

func TestScheduleVM(t *testing.T) {
	ctrlClient := setupClient()
	require.NotNil(t, ctrlClient)

	ipamHelper := &ipam.Helper{}

	proxmoxCluster := infrav1.ProxmoxCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "bar",
		},
		Spec: infrav1.ProxmoxClusterSpec{
			AllowedNodes: []string{"pve1", "pve2", "pve3"},
		},
		Status: infrav1.ProxmoxClusterStatus{
			NodeLocations: &infrav1.NodeLocations{
				ControlPlane: []infrav1.NodeLocation{},
				Workers: []infrav1.NodeLocation{
					{
						Node: "pve1",
						Machine: corev1.LocalObjectReference{
							Name: "foo-machine",
						},
					},
				},
			},
		},
	}

	err := ctrlClient.Create(context.Background(), &proxmoxCluster)
	require.NoError(t, err)

	proxmoxMachine := &infrav1.ProxmoxMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name: "foo-machine",
			Labels: map[string]string{
				"cluster.x-k8s.io/cluster-name": "bar",
			},
		},
		Spec: infrav1.ProxmoxMachineSpec{
			MemoryMiB: new(int32(10)),
		},
	}

	fakeProxmoxClient := proxmoxtest.NewMockClient(t)

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bar",
			Namespace: "default",
		},
	}
	machineScope, err := scope.NewMachineScope(scope.MachineScopeParams{
		Client: ctrlClient,
		Machine: &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "foo-machine",
				Namespace: "default",
			},
		},
		Cluster: cluster,
		InfraCluster: &scope.ClusterScope{
			Cluster:        cluster,
			ProxmoxCluster: &proxmoxCluster,
			ProxmoxClient:  fakeProxmoxClient,
		},
		ProxmoxMachine: proxmoxMachine,
		IPAMHelper:     ipamHelper,
	})
	require.NoError(t, err)

	fakeProxmoxClient.EXPECT().GetReservableMemoryBytes(context.Background(), "pve1", int64(100)).Return(miBytes(60), miBytes(100), nil)
	fakeProxmoxClient.EXPECT().GetReservableMemoryBytes(context.Background(), "pve2", int64(100)).Return(miBytes(20), miBytes(100), nil)
	fakeProxmoxClient.EXPECT().GetReservableMemoryBytes(context.Background(), "pve3", int64(100)).Return(miBytes(20), miBytes(100), nil)

	node, err := ScheduleVM(context.Background(), machineScope)
	require.NoError(t, err)
	require.Equal(t, "pve1", node)
}

func TestToleranceScoringCPUDisabled(t *testing.T) {
	// When cpuAdjustment=0 CPU is not fetched and does not influence scoring;
	// the algorithm degrades to memory-only curve scoring regardless of tolerance hints.
	allowedNodes := []string{"pve1", "pve2", "pve3"}
	availableMem := map[string]uint64{
		"pve1": miBytes(20),
		"pve2": miBytes(30),
		"pve3": miBytes(15),
	}

	proxmoxMachine := &infrav1.ProxmoxMachine{
		Spec: infrav1.ProxmoxMachineSpec{
			MemoryMiB:  ptr.To(int32(8)),
			NumSockets: ptr.To(int32(2)),
			NumCores:   ptr.To(int32(4)),
		},
	}

	cl := fakeResourceClient{memory: availableMem}

	hints := &infrav1.SchedulerHints{
		CPUAdjustment:   ptr.To(int64(0)),
		CPUTolerance:    ptr.To(int64(0)),
		MemoryTolerance: ptr.To(int64(100)),
	}
	node, err := selectNode(context.Background(), cl, proxmoxMachine, nil, allowedNodes, nil, hints)
	require.NoError(t, err)
	require.Equal(t, "pve2", node)
}

func TestScheduleVMWithFailureDomainNodes(t *testing.T) {
	ctrlClient := setupClient()

	proxmoxCluster := infrav1.ProxmoxCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "bar"},
		Spec: infrav1.ProxmoxClusterSpec{
			AllowedNodes: []string{"pve1", "pve2", "pve3"},
			ZoneConfigs: []infrav1.ZoneConfigSpec{
				{
					Zone:  new("zone-a"),
					Nodes: []string{"pve3"},
				},
			},
		},
		Status: infrav1.ProxmoxClusterStatus{
			NodeLocations: &infrav1.NodeLocations{
				Workers: []infrav1.NodeLocation{},
			},
		},
	}
	require.NoError(t, ctrlClient.Create(context.Background(), &proxmoxCluster))

	proxmoxMachine := &infrav1.ProxmoxMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "foo-machine",
			Labels: map[string]string{"cluster.x-k8s.io/cluster-name": "bar"},
		},
		Spec: infrav1.ProxmoxMachineSpec{
			MemoryMiB: new(int32(10)),
		},
	}

	fakeProxmoxClient := proxmoxtest.NewMockClient(t)
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "bar", Namespace: "default"},
	}
	machineScope, err := scope.NewMachineScope(scope.MachineScopeParams{
		Client: ctrlClient,
		Machine: &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "foo-machine", Namespace: "default"},
			Spec:       clusterv1.MachineSpec{FailureDomain: "zone-a"},
		},
		Cluster: cluster,
		InfraCluster: &scope.ClusterScope{
			Cluster:        cluster,
			ProxmoxCluster: &proxmoxCluster,
			ProxmoxClient:  fakeProxmoxClient,
		},
		ProxmoxMachine: proxmoxMachine,
		IPAMHelper:     &ipam.Helper{},
	})
	require.NoError(t, err)
	require.NoError(t, machineScope.FailureDomainError())

	// The failure domain restricts placement to pve3: only pve3 may be queried.
	fakeProxmoxClient.EXPECT().GetReservableMemoryBytes(context.Background(), "pve3", int64(100)).Return(miBytes(60), miBytes(100), nil)

	node, err := ScheduleVM(context.Background(), machineScope)
	require.NoError(t, err)
	require.Equal(t, "pve3", node)
}

func TestScheduleVMIgnoresPlacementsFromOtherZones(t *testing.T) {
	ctrlClient := setupClient()

	// Two zones backed by different Proxmox clusters that happen to use the
	// same node names. Placements recorded for zone-b must not be counted
	// against the same-named node of zone-a.
	proxmoxCluster := infrav1.ProxmoxCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "bar"},
		Spec: infrav1.ProxmoxClusterSpec{
			AllowedNodes: []string{"pve1", "pve2"},
			ZoneConfigs: []infrav1.ZoneConfigSpec{
				{Zone: new("zone-a"), Nodes: []string{"pve1", "pve2"}},
				{Zone: new("zone-b"), Nodes: []string{"pve1", "pve2"}},
			},
		},
		Status: infrav1.ProxmoxClusterStatus{
			NodeLocations: &infrav1.NodeLocations{
				Workers: []infrav1.NodeLocation{
					{Machine: corev1.LocalObjectReference{Name: "a1"}, Node: "pve2", Zone: new("zone-a")},
					{Machine: corev1.LocalObjectReference{Name: "b1"}, Node: "pve1", Zone: new("zone-b")},
					{Machine: corev1.LocalObjectReference{Name: "b2"}, Node: "pve1", Zone: new("zone-b")},
					{Machine: corev1.LocalObjectReference{Name: "b3"}, Node: "pve1", Zone: new("zone-b")},
				},
			},
		},
	}
	require.NoError(t, ctrlClient.Create(context.Background(), &proxmoxCluster))

	proxmoxMachine := &infrav1.ProxmoxMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "foo-machine",
			Labels: map[string]string{"cluster.x-k8s.io/cluster-name": "bar"},
		},
		Spec: infrav1.ProxmoxMachineSpec{
			MemoryMiB: new(int32(10)),
		},
	}

	fakeProxmoxClient := proxmoxtest.NewMockClient(t)
	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "bar", Namespace: "default"},
	}
	machineScope, err := scope.NewMachineScope(scope.MachineScopeParams{
		Client: ctrlClient,
		Machine: &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "foo-machine", Namespace: "default"},
			Spec:       clusterv1.MachineSpec{FailureDomain: "zone-a"},
		},
		Cluster: cluster,
		InfraCluster: &scope.ClusterScope{
			Cluster:        cluster,
			ProxmoxCluster: &proxmoxCluster,
			ProxmoxClient:  fakeProxmoxClient,
		},
		ProxmoxMachine: proxmoxMachine,
		IPAMHelper:     &ipam.Helper{},
	})
	require.NoError(t, err)
	require.NoError(t, machineScope.FailureDomainError())

	fakeProxmoxClient.EXPECT().GetReservableMemoryBytes(context.Background(), "pve1", int64(100)).Return(miBytes(60), miBytes(100), nil)
	fakeProxmoxClient.EXPECT().GetReservableMemoryBytes(context.Background(), "pve2", int64(100)).Return(miBytes(50), miBytes(100), nil)

	// Within zone-a, pve1 holds no machines and pve2 holds one, so pve1 wins.
	// Counting the three zone-b placements on pve1 would flip the decision to
	// pve2 even though that node is the busier one in this zone.
	node, err := ScheduleVM(context.Background(), machineScope)
	require.NoError(t, err)
	require.Equal(t, "pve1", node)
}

func TestToleranceScoringDefaultsFollowMemory(t *testing.T) {
	// Default tolerance: memoryTolerance=0, cpuTolerance=100 -> CPU weight is zero,
	// memory drives the decision. Two nodes with identical CPU headroom but different
	// memory headroom must resolve to the node with more memory.
	allowedNodes := []string{"pve1", "pve2"}

	proxmoxMachine := &infrav1.ProxmoxMachine{
		Spec: infrav1.ProxmoxMachineSpec{
			MemoryMiB:  ptr.To(int32(8)),
			NumSockets: ptr.To(int32(1)),
			NumCores:   ptr.To(int32(4)),
		},
	}

	cl := fakeResourceClient{
		memory:      map[string]uint64{"pve1": miBytes(350), "pve2": miBytes(200)},
		memoryTotal: map[string]uint64{"pve1": miBytes(400), "pve2": miBytes(400)},
		cpus:        map[string]int{"pve1": 50, "pve2": 50},
		cpusTotal:   map[string]int{"pve1": 64, "pve2": 64},
	}

	hints := &infrav1.SchedulerHints{CPUAdjustment: ptr.To(int64(300))}
	node, err := selectNode(context.Background(), cl, proxmoxMachine, nil, allowedNodes, nil, hints)
	require.NoError(t, err)
	require.Equal(t, "pve1", node)
}

func TestToleranceScoringCPUTolerancePicksCPUHeadroom(t *testing.T) {
	// Opposite polarity: memoryTolerance=100 (weight 0), cpuTolerance=0 (weight 1).
	// Two nodes with identical memory headroom but different CPU headroom must resolve
	// to the node with more CPU headroom.
	allowedNodes := []string{"pve1", "pve2"}

	proxmoxMachine := &infrav1.ProxmoxMachine{
		Spec: infrav1.ProxmoxMachineSpec{
			MemoryMiB:  ptr.To(int32(8)),
			NumSockets: ptr.To(int32(1)),
			NumCores:   ptr.To(int32(4)),
		},
	}

	cl := fakeResourceClient{
		memory:      map[string]uint64{"pve1": miBytes(200), "pve2": miBytes(200)},
		memoryTotal: map[string]uint64{"pve1": miBytes(400), "pve2": miBytes(400)},
		cpus:        map[string]int{"pve1": 80, "pve2": 40},
		cpusTotal:   map[string]int{"pve1": 96, "pve2": 96},
	}

	hints := &infrav1.SchedulerHints{
		CPUAdjustment:   ptr.To(int64(300)),
		MemoryTolerance: ptr.To(int64(100)),
		CPUTolerance:    ptr.To(int64(0)),
	}
	node, err := selectNode(context.Background(), cl, proxmoxMachine, nil, allowedNodes, nil, hints)
	require.NoError(t, err)
	require.Equal(t, "pve1", node)
}

func TestToleranceScoringPenalizesOverbook(t *testing.T) {
	// Two nodes with the same physical capacity (32 cores, 200MiB) and the same
	// cpuAdjustment=200. pve1 has usage well inside the physical range; pve2 has
	// already crossed into the overcommit range. With cpuTolerance=0, pve1 must win
	// because headroom in the overcommit range contributes less to the score and
	// the non-linear curve heavily penalizes saturated hosts.
	allowedNodes := []string{"pve1", "pve2"}

	proxmoxMachine := &infrav1.ProxmoxMachine{
		Spec: infrav1.ProxmoxMachineSpec{
			MemoryMiB:  ptr.To(int32(8)),
			NumSockets: ptr.To(int32(1)),
			NumCores:   ptr.To(int32(2)),
		},
	}

	// cpuAdjustment=200 => allocatableCPUs = phys * 2 = 64. Mem mirrors the pattern.
	// pve1: used_cpu=10 (phys range); pve2: used_cpu=40 (already past 32 physical cores).
	cl := fakeResourceClient{
		memory:      map[string]uint64{"pve1": miBytes(150), "pve2": miBytes(120)},
		memoryTotal: map[string]uint64{"pve1": miBytes(400), "pve2": miBytes(400)},
		cpus:        map[string]int{"pve1": 54, "pve2": 24},
		cpusTotal:   map[string]int{"pve1": 64, "pve2": 64},
	}

	hints := &infrav1.SchedulerHints{
		CPUAdjustment:   ptr.To(int64(200)),
		MemoryTolerance: ptr.To(int64(100)),
		CPUTolerance:    ptr.To(int64(0)),
	}
	node, err := selectNode(context.Background(), cl, proxmoxMachine, nil, allowedNodes, nil, hints)
	require.NoError(t, err)
	require.Equal(t, "pve1", node)
}

func TestToleranceScoringHardConstraintCPU(t *testing.T) {
	// Request exceeds the CPU allocatable on every node -> InsufficientCPUError.
	allowedNodes := []string{"pve1", "pve2"}

	proxmoxMachine := &infrav1.ProxmoxMachine{
		Spec: infrav1.ProxmoxMachineSpec{
			MemoryMiB:  ptr.To(int32(8)),
			NumSockets: ptr.To(int32(1)),
			NumCores:   ptr.To(int32(8)),
		},
	}

	cl := fakeResourceClient{
		memory:      map[string]uint64{"pve1": miBytes(500), "pve2": miBytes(500)},
		memoryTotal: map[string]uint64{"pve1": miBytes(500), "pve2": miBytes(500)},
		cpus:        map[string]int{"pve1": 2, "pve2": 4},
		cpusTotal:   map[string]int{"pve1": 16, "pve2": 16},
	}

	hints := &infrav1.SchedulerHints{CPUAdjustment: ptr.To(int64(100))}
	node, err := selectNode(context.Background(), cl, proxmoxMachine, nil, allowedNodes, nil, hints)
	require.ErrorAs(t, err, &InsufficientCPUError{})
	require.Empty(t, node)
}

func TestToleranceScoringHardConstraintMemory(t *testing.T) {
	// Request exceeds the memory available on every node -> InsufficientMemoryError.
	allowedNodes := []string{"pve1"}

	proxmoxMachine := &infrav1.ProxmoxMachine{
		Spec: infrav1.ProxmoxMachineSpec{
			MemoryMiB:  ptr.To(int32(100)),
			NumSockets: ptr.To(int32(1)),
			NumCores:   ptr.To(int32(2)),
		},
	}

	cl := fakeResourceClient{
		memory:      map[string]uint64{"pve1": miBytes(50)},
		memoryTotal: map[string]uint64{"pve1": miBytes(500)},
		cpus:        map[string]int{"pve1": 64},
		cpusTotal:   map[string]int{"pve1": 64},
	}

	hints := &infrav1.SchedulerHints{CPUAdjustment: ptr.To(int64(100))}
	node, err := selectNode(context.Background(), cl, proxmoxMachine, nil, allowedNodes, nil, hints)
	require.ErrorAs(t, err, &InsufficientMemoryError{})
	require.Empty(t, node)
}

func TestToleranceScoringHeterogeneousRegression(t *testing.T) {
	// Regression for issue #724: on a heterogeneous cluster (some nodes with 64 cores,
	// others with 32 cores) scheduling N VMs with balanced tolerance must keep the
	// per-node CPU allocation ratio spread contained, unlike the legacy memory-only scheduler.
	// We schedule 24 identical VMs (2 cores, 8MiB each) across 4 nodes and assert that
	// the max_ratio / min_ratio across nodes stays below 2.0.
	allowedNodes := []string{"big1", "big2", "small1", "small2"}

	// physical capacities; adjustment=300 => allocatable = phys * 3.
	physCPU := map[string]int{
		"big1": 64, "big2": 64, "small1": 32, "small2": 32,
	}
	physMem := map[string]uint64{
		"big1": miBytes(504 * 1024), "big2": miBytes(504 * 1024),
		"small1": miBytes(504 * 1024), "small2": miBytes(504 * 1024),
	}

	availCPU := map[string]int{}
	availMem := map[string]uint64{}
	cpuTotals := map[string]int{}
	memTotals := map[string]uint64{}
	for n, phys := range physCPU {
		cpuTotals[n] = phys * 3
		availCPU[n] = phys * 3
	}
	for n, phys := range physMem {
		memTotals[n] = phys
		availMem[n] = phys
	}

	usedCPU := map[string]int{"big1": 0, "big2": 0, "small1": 0, "small2": 0}

	hints := &infrav1.SchedulerHints{
		CPUAdjustment:   ptr.To(int64(300)),
		MemoryTolerance: ptr.To(int64(0)),
		CPUTolerance:    ptr.To(int64(50)),
	}

	proxmoxMachine := &infrav1.ProxmoxMachine{
		Spec: infrav1.ProxmoxMachineSpec{
			MemoryMiB:  ptr.To(int32(8 * 1024)),
			NumSockets: ptr.To(int32(1)),
			NumCores:   ptr.To(int32(2)),
		},
	}

	var locations []infrav1.NodeLocation
	for i := 0; i < 24; i++ {
		cl := fakeResourceClient{
			memory: availMem, memoryTotal: memTotals,
			cpus: availCPU, cpusTotal: cpuTotals,
		}
		node, err := selectNode(context.Background(), cl, proxmoxMachine, locations, allowedNodes, nil, hints)
		require.NoError(t, err, "round %d", i)
		availCPU[node] -= 2
		availMem[node] -= miBytes(8 * 1024)
		usedCPU[node] += 2
		locations = append(locations, infrav1.NodeLocation{Node: node})
	}

	minRatio, maxRatio := 1e9, 0.0
	for n, used := range usedCPU {
		r := float64(used) / float64(physCPU[n])
		if r < minRatio {
			minRatio = r
		}
		if r > maxRatio {
			maxRatio = r
		}
	}
	t.Logf("CPU allocation per node: %v (ratios min=%.3f max=%.3f spread=%.3fx)", usedCPU, minRatio, maxRatio, maxRatio/minRatio)
	// The legacy memory-only scheduler on this scenario hits ~4.4x spread (see issue #724).
	// The current tolerance-weighted implementation measures at ~1.43x; the bound
	// at 1.5x catches any regression in the scoring curve or pending inflation.
	require.Less(t, maxRatio/minRatio, 1.5,
		"CPU allocation ratio spread too wide: min=%.2f max=%.2f used=%v", minRatio, maxRatio, usedCPU)
}

func TestToleranceScoringAvoidsClumpingOnRapidScale(t *testing.T) {
	// Regression: when multiple VMs are scheduled in rapid succession, the Proxmox
	// resource API lags behind (just-cloned VMs aren't yet reflected in availMem/
	// availCpu). The scheduler must use status.NodeLocations to count placements
	// already committed and inflate the effective usage, so subsequent decisions
	// spread across nodes instead of clumping on the first-chosen winner.
	allowedNodes := []string{"pve1", "pve2", "pve3"}

	proxmoxMachine := &infrav1.ProxmoxMachine{
		Spec: infrav1.ProxmoxMachineSpec{
			MemoryMiB:  ptr.To(int32(8 * 1024)),
			NumSockets: ptr.To(int32(1)),
			NumCores:   ptr.To(int32(2)),
		},
	}

	// Three identical nodes. A naive scheduler that ignores locations would pick
	// the same node every time because the fake client returns constant values.
	cl := fakeResourceClient{
		memory:      map[string]uint64{"pve1": miBytes(100 * 1024), "pve2": miBytes(100 * 1024), "pve3": miBytes(100 * 1024)},
		memoryTotal: map[string]uint64{"pve1": miBytes(128 * 1024), "pve2": miBytes(128 * 1024), "pve3": miBytes(128 * 1024)},
		cpus:        map[string]int{"pve1": 60, "pve2": 60, "pve3": 60},
		cpusTotal:   map[string]int{"pve1": 96, "pve2": 96, "pve3": 96},
	}

	hints := &infrav1.SchedulerHints{
		CPUAdjustment:   ptr.To(int64(300)),
		MemoryTolerance: ptr.To(int64(0)),
		CPUTolerance:    ptr.To(int64(50)),
	}

	placements := map[string]int{}
	var locations []infrav1.NodeLocation
	for i := 0; i < 6; i++ {
		node, err := selectNode(context.Background(), cl, proxmoxMachine, locations, allowedNodes, nil, hints)
		require.NoError(t, err, "round %d", i)
		placements[node]++
		locations = append(locations, infrav1.NodeLocation{Node: node})
	}

	// With 6 VMs across 3 identical nodes we expect perfect spread: 2 each.
	for _, n := range allowedNodes {
		require.Equalf(t, 2, placements[n], "node %s got %d placements, expected even spread (all: %v)", n, placements[n], placements)
	}
}

func TestSelectNodeMemoryQueryError(t *testing.T) {
	proxmoxMachine := &infrav1.ProxmoxMachine{
		Spec: infrav1.ProxmoxMachineSpec{
			MemoryMiB: ptr.To(int32(8)),
		},
	}

	cl := errorResourceClient{
		fakeResourceClient: fakeResourceClient{
			memory: map[string]uint64{"pve1": miBytes(100)},
		},
		memErr: fmt.Errorf("connection refused"),
	}

	node, err := selectNode(context.Background(), cl, proxmoxMachine, nil, []string{"pve1"}, nil, &infrav1.SchedulerHints{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "connection refused")
	require.Empty(t, node)
}

func TestSelectNodeCPUQueryError(t *testing.T) {
	proxmoxMachine := &infrav1.ProxmoxMachine{
		Spec: infrav1.ProxmoxMachineSpec{
			MemoryMiB:  ptr.To(int32(8)),
			NumSockets: ptr.To(int32(1)),
			NumCores:   ptr.To(int32(4)),
		},
	}

	cl := errorResourceClient{
		fakeResourceClient: fakeResourceClient{
			memory: map[string]uint64{"pve1": miBytes(100)},
			cpus:   map[string]int{"pve1": 32},
		},
		cpuErr: fmt.Errorf("node unavailable"),
	}

	hints := &infrav1.SchedulerHints{CPUAdjustment: ptr.To(int64(100))}
	node, err := selectNode(context.Background(), cl, proxmoxMachine, nil, []string{"pve1"}, nil, hints)
	require.Error(t, err)
	require.Contains(t, err.Error(), "node unavailable")
	require.Empty(t, node)
}

func TestToleranceScoringSpreadsBurstOnUnequalNodes(t *testing.T) {
	// The companion of TestToleranceScoringAvoidsClumpingOnRapidScale, which
	// uses three identical nodes. Identical nodes tie on score, and a tie is
	// the easy case. Real nodes never tie: they carry slightly different
	// loads, so the spreading has to come from the scoring itself rather than
	// from a tie-break.
	//
	// The client returns constant values, as Proxmox does while it catches up
	// with a burst of clones. Without a spreading signal every machine reads
	// the same headroom and lands on the same node.
	allowedNodes := []string{"pve1", "pve2", "pve3"}

	proxmoxMachine := &infrav1.ProxmoxMachine{
		Spec: infrav1.ProxmoxMachineSpec{
			MemoryMiB:  ptr.To(int32(8)),
			NumSockets: ptr.To(int32(1)),
			NumCores:   ptr.To(int32(2)),
		},
	}

	cl := fakeResourceClient{
		memory:      map[string]uint64{"pve1": miBytes(300), "pve2": miBytes(290), "pve3": miBytes(280)},
		memoryTotal: map[string]uint64{"pve1": miBytes(400), "pve2": miBytes(400), "pve3": miBytes(400)},
		cpus:        map[string]int{"pve1": 60, "pve2": 58, "pve3": 56},
		cpusTotal:   map[string]int{"pve1": 96, "pve2": 96, "pve3": 96},
	}

	hints := &infrav1.SchedulerHints{
		CPUAdjustment: ptr.To(int64(300)),
		CPUTolerance:  ptr.To(int64(50)),
	}

	const burst = 6

	locations := make([]infrav1.NodeLocation, 0, burst)
	placed := map[string]int{}
	for range burst {
		node, err := selectNode(context.Background(), cl, proxmoxMachine, locations, allowedNodes, nil, hints)
		require.NoError(t, err)
		placed[node]++
		locations = append(locations, infrav1.NodeLocation{Node: node})
	}

	require.Len(t, placed, len(allowedNodes), "every node must take a share, got %v", placed)
	for node, n := range placed {
		require.LessOrEqual(t, n, burst/2, "node %s took %d of %d machines: %v", node, n, burst, placed)
	}
}

func TestScheduleVMUsesZoneSchedulerHints(t *testing.T) {
	ctrlClient := setupClient()

	// The cluster allows 100% of memory, the zone 250%. Zones often sit on
	// hosts of a different size, and one cluster-wide number cannot describe
	// both.
	proxmoxCluster := infrav1.ProxmoxCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "bar"},
		Spec: infrav1.ProxmoxClusterSpec{
			AllowedNodes:   []string{"pve1"},
			SchedulerHints: &infrav1.SchedulerHints{MemoryAdjustment: ptr.To(int64(100))},
			ZoneConfigs: []infrav1.ZoneConfigSpec{
				{
					Zone:           ptr.To("zone-a"),
					Nodes:          []string{"pve3"},
					SchedulerHints: &infrav1.SchedulerHints{MemoryAdjustment: ptr.To(int64(250))},
				},
			},
		},
		Status: infrav1.ProxmoxClusterStatus{
			NodeLocations: &infrav1.NodeLocations{Workers: []infrav1.NodeLocation{}},
		},
	}
	require.NoError(t, ctrlClient.Create(context.Background(), &proxmoxCluster))

	proxmoxMachine := &infrav1.ProxmoxMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "foo-machine",
			Labels: map[string]string{"cluster.x-k8s.io/cluster-name": "bar"},
		},
		Spec: infrav1.ProxmoxMachineSpec{MemoryMiB: ptr.To(int32(10))},
	}

	fakeProxmoxClient := proxmoxtest.NewMockClient(t)
	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "bar", Namespace: "default"}}
	machineScope, err := scope.NewMachineScope(scope.MachineScopeParams{
		Client: ctrlClient,
		Machine: &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "foo-machine", Namespace: "default"},
			Spec:       clusterv1.MachineSpec{FailureDomain: "zone-a"},
		},
		Cluster: cluster,
		InfraCluster: &scope.ClusterScope{
			Cluster:        cluster,
			ProxmoxCluster: &proxmoxCluster,
			ProxmoxClient:  fakeProxmoxClient,
		},
		ProxmoxMachine: proxmoxMachine,
		IPAMHelper:     &ipam.Helper{},
	})
	require.NoError(t, err)
	require.NoError(t, machineScope.FailureDomainError())

	// The adjustment reaches Proxmox as an argument, so the expectation is
	// the assertion: without the override the client is called with 100 and
	// the mock fails on an unexpected call.
	fakeProxmoxClient.EXPECT().
		GetReservableMemoryBytes(context.Background(), "pve3", int64(250)).
		Return(miBytes(60), miBytes(100), nil)

	node, err := ScheduleVM(context.Background(), machineScope)
	require.NoError(t, err)
	require.Equal(t, "pve3", node)
}

func TestToleranceScoringNormalisesAcrossAdjustments(t *testing.T) {
	// Memory and CPU almost always run at different adjustments. Each
	// resource score is normalised against a completely empty node, so equal
	// tolerances mean equal influence whatever those adjustments are.
	//
	// Without the normalisation the maximum score is
	// (1 + overbookDiscount*(alloc/phys - 1))^2: 1.0 at 100% but 4.0 at 300%.
	// CPU would then carry four times its intended weight, and pve2 would win
	// on a CPU edge that is smaller, in relative terms, than pve1's lead on
	// memory.
	allowedNodes := []string{"pve1", "pve2"}

	proxmoxMachine := &infrav1.ProxmoxMachine{
		Spec: infrav1.ProxmoxMachineSpec{
			MemoryMiB:  ptr.To(int32(8)),
			NumSockets: ptr.To(int32(1)),
			NumCores:   ptr.To(int32(4)),
		},
	}

	cl := fakeResourceClient{
		memory:      map[string]uint64{"pve1": miBytes(350), "pve2": miBytes(250)},
		memoryTotal: map[string]uint64{"pve1": miBytes(400), "pve2": miBytes(400)},
		cpus:        map[string]int{"pve1": 40, "pve2": 70},
		cpusTotal:   map[string]int{"pve1": 96, "pve2": 96},
	}

	hints := &infrav1.SchedulerHints{
		MemoryAdjustment: ptr.To(int64(100)),
		CPUAdjustment:    ptr.To(int64(300)),
		MemoryTolerance:  ptr.To(int64(50)),
		CPUTolerance:     ptr.To(int64(50)),
	}

	node, err := selectNode(context.Background(), cl, proxmoxMachine, nil, allowedNodes, nil, hints)
	require.NoError(t, err)
	require.Equal(t, "pve1", node)
}

func TestInsufficientMemoryError_Error(t *testing.T) {
	err := InsufficientMemoryError{
		node:      "pve1",
		available: 10,
		requested: 20,
	}
	require.Equal(t, "cannot reserve 20B of memory on node pve1: 10B available memory left", err.Error())
}

func TestInsufficientCPUError_Error(t *testing.T) {
	err := InsufficientCPUError{
		node:      "pve1",
		available: 2,
		requested: 8,
	}
	require.Equal(t, "cannot reserve 8 CPU cores on node pve1: 2 available cores left", err.Error())
}

func setupClient() client.Client {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(infrav1.AddToScheme(scheme))
	utilruntime.Must(infrav1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	return fakeClient
}

func giBytes(in int) uint64 {
	return uint64(in) * 1024 * 1024 * 1024
}

// twoCoreMachine is the request shared by the scoring-scale tests below:
// 2GiB of memory and 2 CPU cores.
func twoCoreMachine() *infrav1.ProxmoxMachine {
	return &infrav1.ProxmoxMachine{
		Spec: infrav1.ProxmoxMachineSpec{
			MemoryMiB:  ptr.To(int32(2048)),
			NumSockets: ptr.To(int32(1)),
			NumCores:   ptr.To(int32(2)),
		},
	}
}

// TestScoringMeasuresSaturationAgainstTheAllocatableCap pins the bottom of the
// score when an adjustment below 100 caps allocation under the physical size.
//
// pve2 has spent 20 of the 25GiB it is allowed to hand out, pve1 only 5. Judged
// against the 100GiB the hosts physically carry, pve2 still looks 78% free and
// its better CPU headroom carries the decision. Judged against the cap the
// operator actually set, pve2 is nearly full and pve1 wins — which is the point
// of underprovisioning: the reserved headroom is not available capacity.
func TestScoringMeasuresSaturationAgainstTheAllocatableCap(t *testing.T) {
	cl := fakeResourceClient{
		memory:      map[string]uint64{"pve1": giBytes(20), "pve2": giBytes(5)},
		memoryTotal: map[string]uint64{"pve1": giBytes(25), "pve2": giBytes(25)},
		cpus:        map[string]int{"pve1": 6, "pve2": 21},
		cpusTotal:   map[string]int{"pve1": 32, "pve2": 32},
	}

	node, err := selectNode(context.Background(), cl, twoCoreMachine(), nil,
		[]string{"pve1", "pve2"}, nil, &infrav1.SchedulerHints{
			MemoryAdjustment: ptr.To(int64(25)),
			CPUAdjustment:    ptr.To(int64(100)),
			MemoryTolerance:  ptr.To(int64(0)),
			CPUTolerance:     ptr.To(int64(0)),
		})

	require.NoError(t, err)
	require.Equal(t, "pve1", node)
}

// TestDisabledMemoryDoesNotDecide checks that memoryAdjustment 0 means what the
// field says: memory stops being a scheduling constraint.
//
// With it set, the client reports the whole node as both allocatable and
// available. Scoring that would rank nodes by raw size — the 400GiB pve1 scores
// 0.990 against the 100GiB pve2's 0.960 on a resource that is switched off —
// and that gap is enough to outweigh pve2's real CPU advantage.
func TestDisabledMemoryDoesNotDecide(t *testing.T) {
	cl := fakeResourceClient{
		memory:      map[string]uint64{"pve1": giBytes(400), "pve2": giBytes(100)},
		memoryTotal: map[string]uint64{"pve1": giBytes(400), "pve2": giBytes(100)},
		cpus:        map[string]int{"pve1": 10, "pve2": 11},
		cpusTotal:   map[string]int{"pve1": 32, "pve2": 32},
	}

	node, err := selectNode(context.Background(), cl, twoCoreMachine(), nil,
		[]string{"pve1", "pve2"}, nil, &infrav1.SchedulerHints{
			MemoryAdjustment: ptr.To(int64(0)),
			CPUAdjustment:    ptr.To(int64(100)),
			MemoryTolerance:  ptr.To(int64(0)),
			CPUTolerance:     ptr.To(int64(0)),
		})

	require.NoError(t, err)
	require.Equal(t, "pve2", node, "the node with more free CPU must win when memory is disabled")
}

// TestPendingBreaksTieWhenNothingElseDoes covers the last resort. With both
// adjustments at 0 every score is 0 and, on identical hardware, the reported
// availability is identical too — so without counting placements a burst would
// pick the same node every time.
func TestPendingBreaksTieWhenNothingElseDoes(t *testing.T) {
	cl := fakeResourceClient{
		memory:      map[string]uint64{"pve1": giBytes(100), "pve2": giBytes(100)},
		memoryTotal: map[string]uint64{"pve1": giBytes(100), "pve2": giBytes(100)},
	}
	locations := []infrav1.NodeLocation{{Node: "pve1"}}

	node, err := selectNode(context.Background(), cl, twoCoreMachine(), locations,
		[]string{"pve1", "pve2"}, nil, &infrav1.SchedulerHints{
			MemoryAdjustment: ptr.To(int64(0)),
			CPUAdjustment:    ptr.To(int64(0)),
		})

	require.NoError(t, err)
	require.Equal(t, "pve2", node)
}

// TestBuildInsufficientErrorOnEmptyNodeList guards the indexing below it:
// callers gate on a non-empty allowed-node list, and this must report that
// rather than panic if one ever stops.
func TestBuildInsufficientErrorOnEmptyNodeList(t *testing.T) {
	require.ErrorContains(t, buildInsufficientError(nil, miBytes(8), 2), "allowed node list is empty")
}

// TestOversizedCPURequestDoesNotWrap covers an int32 product that overflows.
// numSockets and numCores carry no upper bound, and 65536 x 65536 is exactly
// 2^32: as an int32 the request reads 0, slips past the hard-fit check and
// schedules a VM Proxmox cannot possibly start.
func TestOversizedCPURequestDoesNotWrap(t *testing.T) {
	cl := fakeResourceClient{
		memory:      map[string]uint64{"pve1": giBytes(100)},
		memoryTotal: map[string]uint64{"pve1": giBytes(100)},
		cpus:        map[string]int{"pve1": 64},
		cpusTotal:   map[string]int{"pve1": 64},
	}
	machine := &infrav1.ProxmoxMachine{
		Spec: infrav1.ProxmoxMachineSpec{
			MemoryMiB:  ptr.To(int32(2048)),
			NumSockets: ptr.To(int32(65536)),
			NumCores:   ptr.To(int32(65536)),
		},
	}

	node, err := selectNode(context.Background(), cl, machine, nil,
		[]string{"pve1"}, nil, &infrav1.SchedulerHints{CPUAdjustment: ptr.To(int64(100))})

	require.ErrorAs(t, err, &InsufficientCPUError{})
	require.Empty(t, node)
}

// TestNegativeCPURequestDoesNotInflateScore is the other side of the wrap:
// 50000 x 50000 reads negative as an int32, which would subtract from the
// node's usage and lift its score above the 1.0 the curve is bounded by.
func TestNegativeCPURequestDoesNotInflateScore(t *testing.T) {
	cl := fakeResourceClient{
		memory:      map[string]uint64{"pve1": giBytes(100)},
		memoryTotal: map[string]uint64{"pve1": giBytes(100)},
		cpus:        map[string]int{"pve1": 64},
		cpusTotal:   map[string]int{"pve1": 64},
	}
	machine := &infrav1.ProxmoxMachine{
		Spec: infrav1.ProxmoxMachineSpec{
			MemoryMiB:  ptr.To(int32(2048)),
			NumSockets: ptr.To(int32(50000)),
			NumCores:   ptr.To(int32(50000)),
		},
	}

	_, err := selectNode(context.Background(), cl, machine, nil,
		[]string{"pve1"}, nil, &infrav1.SchedulerHints{CPUAdjustment: ptr.To(int64(100))})

	require.ErrorAs(t, err, &InsufficientCPUError{})
}

// zonedCluster builds the node->zone map the scheduler receives, the way
// ScheduleVM builds it from spec.zoneConfig.
func zonedNodes(zones map[string][]string) map[string]string {
	out := map[string]string{}
	for zone, nodes := range zones {
		for _, n := range nodes {
			out[n] = zone
		}
	}
	return out
}

func evenNodes(names ...string) fakeResourceClient {
	mem, memTotal := map[string]uint64{}, map[string]uint64{}
	for _, n := range names {
		mem[n], memTotal[n] = giBytes(64), giBytes(64)
	}
	return fakeResourceClient{memory: mem, memoryTotal: memTotal}
}

// TestSelectNodeBalancesAcrossAvailabilityZones is the scenario from the
// round-robin-per-zone work in ElmecOSS PR #2 (R34SGA), itself the consensus
// from upstream discussion #896: az-1 has two nodes and
// az-2 one, so counting placements per node lets the bigger zone absorb the
// first two VMs while the other stays empty. Zones are failure domains, so
// that is the one distribution they exist to prevent.
func TestSelectNodeBalancesAcrossAvailabilityZones(t *testing.T) {
	nodeZone := zonedNodes(map[string][]string{"az-1": {"pve1", "pve2"}, "az-2": {"pve3"}})
	allowed := []string{"pve1", "pve2", "pve3"}

	locations := make([]infrav1.NodeLocation, 0, 4)
	perZone := map[string]int{}

	for range 4 {
		node, err := selectNode(context.Background(), evenNodes(allowed...), twoCoreMachine(),
			locations, allowed, nodeZone, &infrav1.SchedulerHints{})
		require.NoError(t, err)
		perZone[nodeZone[node]]++
		// no failure domain on the machine: the location records no zone
		locations = append(locations, infrav1.NodeLocation{Node: node})
	}

	require.Equal(t, 2, perZone["az-1"], "the two-node zone must not take more than its share")
	require.Equal(t, 2, perZone["az-2"])
}

// Within one zone the score decides, so a multi-node zone still spreads over
// its own nodes rather than stacking everything on the first one.
func TestSelectNodeBalancesNodesWithinAvailabilityZone(t *testing.T) {
	nodeZone := zonedNodes(map[string][]string{"az-1": {"pve1", "pve2"}, "az-2": {"pve3", "pve4"}})
	allowed := []string{"pve1", "pve2", "pve3", "pve4"}

	locations := make([]infrav1.NodeLocation, 0, 4)
	perNode := map[string]int{}

	for range 4 {
		node, err := selectNode(context.Background(), evenNodes(allowed...), twoCoreMachine(),
			locations, allowed, nodeZone, &infrav1.SchedulerHints{})
		require.NoError(t, err)
		perNode[node]++
		locations = append(locations, infrav1.NodeLocation{Node: node})
	}

	for _, n := range allowed {
		require.Equal(t, 1, perNode[n], "node %s should have taken exactly one VM", n)
	}
}

// Balancing must stay inert without zones, otherwise placement counts would
// outrank the capacity score for every cluster that has no zoneConfig.
func TestZoneBalancingIsInertWithoutZones(t *testing.T) {
	allowed := []string{"pve1", "pve2"}
	cl := fakeResourceClient{
		// pve1 is the roomier node and must keep winning on score alone,
		// even once it holds more placements than pve2.
		memory:      map[string]uint64{"pve1": giBytes(64), "pve2": giBytes(8)},
		memoryTotal: map[string]uint64{"pve1": giBytes(64), "pve2": giBytes(8)},
	}
	locations := []infrav1.NodeLocation{{Node: "pve1"}, {Node: "pve1"}}

	node, err := selectNode(context.Background(), cl, twoCoreMachine(), locations, allowed, nil,
		&infrav1.SchedulerHints{})

	require.NoError(t, err)
	require.Equal(t, "pve1", node)
}

func TestNodeToZone(t *testing.T) {
	zoneConfigs := []infrav1.ZoneConfigSpec{
		{Zone: ptr.To("az-1"), Nodes: []string{"pve1", "pve2"}},
		{Zone: ptr.To("az-2"), Nodes: []string{"pve3"}},
	}

	got := nodeToZone(zoneConfigs, []string{"pve1", "pve3", "pve9"})

	require.Equal(t, map[string]string{"pve1": "az-1", "pve3": "az-2"}, got,
		"a node no zone lists is left out, not mapped to itself")
	require.Nil(t, nodeToZone(nil, []string{"pve1"}), "no zones configured, no map")
}
