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
		// initial round-robin: everyone has enough memory
		"pve2", "pve1", "pve3",
		// second round-robin: pve3 out of memory
		"pve2", "pve1", "pve2",
	}

	for i, expectedNode := range expectedNodes {
		t.Run(fmt.Sprintf("round %d", i+1), func(t *testing.T) {
			proxmoxMachine := &infrav1.ProxmoxMachine{
				Spec: infrav1.ProxmoxMachineSpec{
					MemoryMiB: &requestMiB,
				},
			}

			cl := fakeResourceClient{memory: availableMem}

			node, err := selectNode(context.Background(), cl, proxmoxMachine, locations, allowedNodes, &infrav1.SchedulerHints{})
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

		node, err := selectNode(context.Background(), cl, proxmoxMachine, locations, allowedNodes, &infrav1.SchedulerHints{})
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
		// initial round-robin: everyone has enough memory
		"pve2", "pve1", "pve3",
		// second round-robin: pve3 out of memory
		"pve2", "pve1", "pve2",
		// third round-robin: pve1 and pve2 has room for one more VM each
		"pve1", "pve2",
	}

	for i, expectedNode := range expectedNodes {
		t.Run(fmt.Sprintf("round %d", i+1), func(t *testing.T) {
			proxmoxMachine := &infrav1.ProxmoxMachine{
				Spec: infrav1.ProxmoxMachineSpec{
					MemoryMiB: &requestMiB,
				},
			}

			cl := fakeResourceClient{memory: availableMem}

			node, err := selectNode(context.Background(), cl, proxmoxMachine, locations, allowedNodes, &infrav1.SchedulerHints{})
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

		node, err := selectNode(context.Background(), cl, proxmoxMachine, locations, allowedNodes, &infrav1.SchedulerHints{})
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
			MemoryMiB: ptr.To(int32(10)),
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
	require.Equal(t, "pve2", node)
}

func TestSelectNodeCPUDisabled(t *testing.T) {
	// When cpuAdjustment=0 (default), the scheduler should use round-robin (legacy behavior).
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

	hints := &infrav1.SchedulerHints{CPUAdjustment: ptr.To(int64(0))}
	node, err := selectNode(context.Background(), cl, proxmoxMachine, nil, allowedNodes, hints)
	require.NoError(t, err)
	// Round-robin with no existing VMs picks the node with most memory.
	require.Equal(t, "pve2", node)
}

func TestSelectNodeWithCPUAwareness(t *testing.T) {
	// Heterogeneous cluster: pve1 has more CPU headroom, pve2 has less.
	// With CPU-aware scheduling, VMs should prefer pve1.
	allowedNodes := []string{"pve1", "pve2"}

	proxmoxMachine := &infrav1.ProxmoxMachine{
		Spec: infrav1.ProxmoxMachineSpec{
			MemoryMiB:  ptr.To(int32(16)),
			NumSockets: ptr.To(int32(1)),
			NumCores:   ptr.To(int32(8)),
		},
	}

	// pve1: 64 cores * 300% = 192 allocatable, 100 used => 92 available. sat_cpu_after = (100+8)/192 = 56.2%
	// pve2: 32 cores * 300% = 96 allocatable, 20 used => 76 available. sat_cpu_after = (20+8)/96 = 29.2%
	// Both have equal RAM. pve2 wins (lower saturation).
	cl := fakeResourceClient{
		memory:      map[string]uint64{"pve1": miBytes(400), "pve2": miBytes(400)},
		memoryTotal: map[string]uint64{"pve1": miBytes(500), "pve2": miBytes(500)},
		cpus:        map[string]int{"pve1": 92, "pve2": 76},
		cpusTotal:   map[string]int{"pve1": 192, "pve2": 96},
	}

	hints := &infrav1.SchedulerHints{CPUAdjustment: ptr.To(int64(300))}
	node, err := selectNode(context.Background(), cl, proxmoxMachine, nil, allowedNodes, hints)
	require.NoError(t, err)
	// pve2 has lower CPU saturation (29.2% vs 56.2%), so it wins.
	require.Equal(t, "pve2", node)
}

func TestSelectNodeSaturationPrefersBalanced(t *testing.T) {
	// pve1: lots of RAM left but tight on CPU
	// pve2: moderate on both
	// Saturation should pick pve2 (better balanced).
	allowedNodes := []string{"pve1", "pve2"}

	proxmoxMachine := &infrav1.ProxmoxMachine{
		Spec: infrav1.ProxmoxMachineSpec{
			MemoryMiB:  ptr.To(int32(8)),
			NumSockets: ptr.To(int32(1)),
			NumCores:   ptr.To(int32(4)),
		},
	}

	// pve1: tons of RAM but almost out of CPU. cpu_sat_after = (96-5+4)/96 = 99%. Bottleneck = 99%.
	// pve2: moderate RAM, plenty of CPU. cpu_sat_after = (96-40+4)/96 = 62.5%. Bottleneck = 62.5%.
	cl := fakeResourceClient{
		memory:      map[string]uint64{"pve1": miBytes(500), "pve2": miBytes(100)},
		memoryTotal: map[string]uint64{"pve1": miBytes(600), "pve2": miBytes(200)},
		cpus:        map[string]int{"pve1": 5, "pve2": 40},
		cpusTotal:   map[string]int{"pve1": 96, "pve2": 96},
	}

	hints := &infrav1.SchedulerHints{CPUAdjustment: ptr.To(int64(300))}
	node, err := selectNode(context.Background(), cl, proxmoxMachine, nil, allowedNodes, hints)
	require.NoError(t, err)
	require.Equal(t, "pve2", node)
}

func TestSelectNodeInsufficientCPU(t *testing.T) {
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
	node, err := selectNode(context.Background(), cl, proxmoxMachine, nil, allowedNodes, hints)
	require.ErrorAs(t, err, &InsufficientCPUError{})
	require.Empty(t, node)
}

func TestSelectNodeInsufficientMemoryWithCPU(t *testing.T) {
	// Both resources enabled, but memory is the bottleneck.
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
	node, err := selectNode(context.Background(), cl, proxmoxMachine, nil, allowedNodes, hints)
	require.ErrorAs(t, err, &InsufficientMemoryError{})
	require.Empty(t, node)
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

	node, err := selectNode(context.Background(), cl, proxmoxMachine, nil, []string{"pve1"}, &infrav1.SchedulerHints{})
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
	node, err := selectNode(context.Background(), cl, proxmoxMachine, nil, []string{"pve1"}, hints)
	require.Error(t, err)
	require.Contains(t, err.Error(), "node unavailable")
	require.Empty(t, node)
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
