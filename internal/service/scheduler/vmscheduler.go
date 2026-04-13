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

// Package scheduler implements scheduling algorithms for Proxmox VMs.
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/go-logr/logr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/cluster-api/util"

	infrav1 "github.com/ionos-cloud/cluster-api-provider-proxmox/api/v1alpha2"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/scope"
)

// InsufficientMemoryError is used when the scheduler cannot assign a VM to a node because it would
// exceed the node's memory limit.
type InsufficientMemoryError struct {
	node      string
	available uint64
	requested uint64
}

func (err InsufficientMemoryError) Error() string {
	return fmt.Sprintf("cannot reserve %dB of memory on node %s: %dB available memory left",
		err.requested, err.node, err.available)
}

// InsufficientCPUError is used when the scheduler cannot assign a VM to a node because it would
// exceed the node's CPU core limit.
type InsufficientCPUError struct {
	node      string
	available int
	requested int
}

func (err InsufficientCPUError) Error() string {
	return fmt.Sprintf("cannot reserve %d CPU cores on node %s: %d available cores left",
		err.requested, err.node, err.available)
}

// ScheduleVM decides which node to a ProxmoxMachine should be scheduled on.
// It requires the machine's ProxmoxCluster to have at least 1 allowed node.
func ScheduleVM(ctx context.Context, machineScope *scope.MachineScope) (string, error) {
	client := machineScope.InfraCluster.ProxmoxClient
	// Use the default allowed nodes from the ProxmoxCluster.
	allowedNodes := machineScope.InfraCluster.ProxmoxCluster.Spec.AllowedNodes
	schedulerHints := machineScope.InfraCluster.ProxmoxCluster.Spec.SchedulerHints
	locations := machineScope.InfraCluster.ProxmoxCluster.Status.NodeLocations.Workers
	if util.IsControlPlaneMachine(machineScope.Machine) {
		locations = machineScope.InfraCluster.ProxmoxCluster.Status.NodeLocations.ControlPlane
	}

	// If ProxmoxMachine defines allowedNodes use them instead
	if len(machineScope.ProxmoxMachine.Spec.AllowedNodes) > 0 {
		allowedNodes = machineScope.ProxmoxMachine.Spec.AllowedNodes
	}

	return selectNode(ctx, client, machineScope.ProxmoxMachine, locations, allowedNodes, schedulerHints)
}

func selectNode(
	ctx context.Context,
	client resourceClient,
	machine *infrav1.ProxmoxMachine,
	locations []infrav1.NodeLocation,
	allowedNodes []string,
	schedulerHints *infrav1.SchedulerHints,
) (string, error) {
	cpuAdjustment := schedulerHints.GetCPUAdjustment()
	cpuAware := cpuAdjustment > 0

	nodes := make([]nodeInfo, len(allowedNodes))
	for i, nodeName := range allowedNodes {
		mem, allocMem, err := client.GetReservableMemoryBytes(ctx, nodeName, schedulerHints.GetMemoryAdjustment())
		if err != nil {
			return "", err
		}

		var cpus, allocCPUs int
		if cpuAware {
			cpus, allocCPUs, err = client.GetReservableCPUCores(ctx, nodeName, cpuAdjustment)
			if err != nil {
				return "", err
			}
		}

		nodes[i] = nodeInfo{
			Name:              nodeName,
			AvailableMemory:   mem,
			AllocatableMemory: allocMem,
			AvailableCPUs:     cpus,
			AllocatableCPUs:   allocCPUs,
		}
	}

	requestedMemory := uint64(ptr.Deref(machine.Spec.MemoryMiB, 0)) * 1024 * 1024
	requestedCPUs := int(ptr.Deref(machine.Spec.NumSockets, 0)) * int(ptr.Deref(machine.Spec.NumCores, 0))

	if cpuAware {
		return selectNodeBySaturation(ctx, nodes, locations, requestedMemory, requestedCPUs)
	}

	return selectNodeByRoundRobin(ctx, nodes, locations, requestedMemory)
}

// selectNodeByRoundRobin is the legacy scheduling algorithm: round-robin by VM count with memory as the only constraint.
func selectNodeByRoundRobin(
	ctx context.Context,
	nodes []nodeInfo,
	locations []infrav1.NodeLocation,
	requestedMemory uint64,
) (string, error) {
	byMemory := sortByAvailableMemory(nodes)
	sort.Sort(byMemory)

	if requestedMemory > byMemory[0].AvailableMemory {
		return "", InsufficientMemoryError{
			node:      byMemory[0].Name,
			available: byMemory[0].AvailableMemory,
			requested: requestedMemory,
		}
	}

	nodeCounter := make(map[string]int)
	for _, nl := range locations {
		nodeCounter[nl.Node]++
	}

	for i, info := range byMemory {
		info.ScheduledVMs = nodeCounter[info.Name]
		byMemory[i] = info
	}

	byReplicas := make(sortByReplicas, len(byMemory))
	copy(byReplicas, byMemory)
	sort.Sort(byReplicas)

	decision := byMemory[0].Name
	for _, info := range byReplicas {
		if requestedMemory < info.AvailableMemory {
			decision = info.Name
			break
		}
	}

	if logger := logr.FromContextOrDiscard(ctx); logger.V(4).Enabled() {
		logger.Info("Scheduler decision (round-robin)",
			"byReplicas", byReplicas.String(),
			"byMemory", byMemory.String(),
			"requestedMemory", requestedMemory,
			"resultNode", decision,
		)
	}

	return decision, nil
}

// selectNodeBySaturation selects the node with the lowest bottleneck saturation ratio.
// The bottleneck is the maximum of CPU saturation and memory saturation for each node.
// This ensures VMs are distributed proportionally to each node's actual capacity.
func selectNodeBySaturation(
	ctx context.Context,
	nodes []nodeInfo,
	locations []infrav1.NodeLocation,
	requestedMemory uint64,
	requestedCPUs int,
) (string, error) {
	// Filter nodes that can fit the VM on both dimensions.
	var candidates []nodeInfo
	for _, n := range nodes {
		memFits := requestedMemory <= n.AvailableMemory
		cpuFits := requestedCPUs <= n.AvailableCPUs

		if memFits && cpuFits {
			candidates = append(candidates, n)
		}
	}

	if len(candidates) == 0 {
		// Find the best error to report: which resource is the tightest?
		sort.Sort(sortByAvailableMemory(nodes))
		if requestedMemory > nodes[0].AvailableMemory {
			return "", InsufficientMemoryError{
				node:      nodes[0].Name,
				available: nodes[0].AvailableMemory,
				requested: requestedMemory,
			}
		}
		// Memory fits somewhere but CPU doesn't — find node with most CPU headroom.
		bestCPU := nodes[0]
		for _, n := range nodes[1:] {
			if n.AvailableCPUs > bestCPU.AvailableCPUs {
				bestCPU = n
			}
		}
		return "", InsufficientCPUError{
			node:      bestCPU.Name,
			available: bestCPU.AvailableCPUs,
			requested: requestedCPUs,
		}
	}

	// Compute the bottleneck saturation for each candidate after placement.
	// saturation = (allocated + requested) / allocatable = 1 - (available - requested) / allocatable
	// The bottleneck is the resource with the highest saturation (closest to full).
	// We pick the node with the lowest bottleneck (least saturated overall).
	type scored struct {
		nodeInfo
		bottleneck float64
	}

	var scoredCandidates []scored
	for _, n := range candidates {
		cpuSat := float64(0)
		if n.AllocatableCPUs > 0 {
			cpuSat = 1.0 - float64(n.AvailableCPUs-requestedCPUs)/float64(n.AllocatableCPUs)
		}

		memSat := float64(0)
		if n.AllocatableMemory > 0 {
			memSat = 1.0 - float64(n.AvailableMemory-requestedMemory)/float64(n.AllocatableMemory)
		}

		// Bottleneck is the tighter resource (highest saturation).
		bottleneck := memSat
		if cpuSat > memSat {
			bottleneck = cpuSat
		}

		scoredCandidates = append(scoredCandidates, scored{nodeInfo: n, bottleneck: bottleneck})
	}

	// Sort by bottleneck ascending (least saturated first).
	sort.Slice(scoredCandidates, func(i, j int) bool {
		return scoredCandidates[i].bottleneck < scoredCandidates[j].bottleneck
	})

	decision := scoredCandidates[0].Name

	if logger := logr.FromContextOrDiscard(ctx); logger.V(4).Enabled() {
		type logEntry struct {
			Node  string  `json:"node"`
			Mem   uint64  `json:"availMem"`
			CPU   int     `json:"availCpu"`
			Score float64 `json:"score"`
		}
		entries := make([]logEntry, len(scoredCandidates))
		for i, sc := range scoredCandidates {
			entries[i] = logEntry{Node: sc.Name, Mem: sc.AvailableMemory, CPU: sc.AvailableCPUs, Score: sc.bottleneck}
		}
		data, _ := json.Marshal(entries)
		logger.Info("Scheduler decision (saturation)",
			"candidates", string(data),
			"requestedMemory", requestedMemory,
			"requestedCPUs", requestedCPUs,
			"resultNode", decision,
		)
	}

	return decision, nil
}

type resourceClient interface {
	GetReservableMemoryBytes(context.Context, string, int64) (available uint64, allocatable uint64, err error)
	GetReservableCPUCores(context.Context, string, int64) (available int, allocatable int, err error)
}

type nodeInfo struct {
	Name              string `json:"node"`
	AvailableMemory   uint64 `json:"mem"`
	AllocatableMemory uint64 `json:"allocMem"`
	AvailableCPUs     int    `json:"cpu"`
	AllocatableCPUs   int    `json:"allocCpu"`
	ScheduledVMs      int    `json:"vms"`
}

type sortByReplicas []nodeInfo

func (a sortByReplicas) Len() int      { return len(a) }
func (a sortByReplicas) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a sortByReplicas) Less(i, j int) bool {
	return a[i].ScheduledVMs < a[j].ScheduledVMs
}

func (a sortByReplicas) String() string {
	o, _ := json.Marshal(a)
	return string(o)
}

type sortByAvailableMemory []nodeInfo

func (a sortByAvailableMemory) Len() int      { return len(a) }
func (a sortByAvailableMemory) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a sortByAvailableMemory) Less(i, j int) bool {
	// more available memory = lower index
	return a[i].AvailableMemory > a[j].AvailableMemory
}

func (a sortByAvailableMemory) String() string {
	o, _ := json.Marshal(a)
	return string(o)
}
