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
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/go-logr/logr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/cluster-api/util"

	infrav1 "github.com/ionos-cloud/cluster-api-provider-proxmox/api/v1alpha2"
	"github.com/ionos-cloud/cluster-api-provider-proxmox/pkg/scope"
)

const (
	// overbookDiscount is the per-unit score of headroom in the overcommit range,
	// relative to 1 point per unit in the physical range.
	overbookDiscount = 0.5
	// saturationExponent shapes the per-resource score as a power curve, so that
	// nodes closer to saturation score disproportionately less than empty nodes.
	saturationExponent = 2.0
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
	client := machineScope.ProxmoxClient()
	schedulerHints := machineScope.SchedulerHints()
	locations := machineScope.InfraCluster.ProxmoxCluster.Status.NodeLocations.Workers
	if util.IsControlPlaneMachine(machineScope.Machine) {
		locations = machineScope.InfraCluster.ProxmoxCluster.Status.NodeLocations.ControlPlane
	}

	locations = locationsInZone(locations, machineScope.Zone())

	return selectNode(ctx, client, machineScope.ProxmoxMachine, locations, machineScope.AllowedNodes(), schedulerHints)
}

// locationsInZone keeps only the placements that belong to the given zone.
//
// Node names are only unique within a single Proxmox cluster, and a zone may be
// backed by its own one. Counting placements by node name alone would therefore
// merge the tallies of same-named nodes living in different zones and skew the
// spread in both.
//
// Entries recorded before zones existed carry no zone and are kept for the
// default zone only, which is where those machines were placed.
func locationsInZone(locations []infrav1.NodeLocation, zone infrav1.Zone) []infrav1.NodeLocation {
	want := ptr.Deref(zone, "")

	inZone := make([]infrav1.NodeLocation, 0, len(locations))
	for _, loc := range locations {
		if ptr.Deref(loc.Zone, "") == want {
			inZone = append(inZone, loc)
		}
	}

	return inZone
}

// selectNode picks a node from the allowed list for a new VM. It always runs
// the same scoring algorithm: each candidate node gets a non-linear score
// based on remaining headroom after hypothetical placement, with the physical
// range worth more than the overcommit range and near-saturation penalized
// quadratically. Tolerance values are inverted into weights: higher tolerance
// for a resource means it contributes less to the decision (= the operator is
// fine saturating it). The node with the highest total score wins.
//
// Existing placements recorded in status.NodeLocations inflate the effective
// usage in the score calculation, so rapid-fire scheduling — Proxmox's
// resource API takes seconds to reflect just-cloned VMs — spreads across
// nodes instead of clumping on the first-chosen winner. The inflation only
// applies to the score, not to the hard-fit check, so already-up-to-date
// availability data is never double-counted.
//
// When cpuAdjustment is 0, CPU information is not fetched, CPU is not used
// as a hard-fit constraint, and CPU does not contribute to the score; the
// algorithm degrades to memory-only scoring with the same curve.
func selectNode(
	ctx context.Context,
	client resourceClient,
	machine *infrav1.ProxmoxMachine,
	locations []infrav1.NodeLocation,
	allowedNodes []string,
	schedulerHints *infrav1.SchedulerHints,
) (string, error) {
	memoryAdjustment := schedulerHints.GetMemoryAdjustment()
	cpuAdjustment := schedulerHints.GetCPUAdjustment()
	memEnabled := memoryAdjustment > 0
	cpuEnabled := cpuAdjustment > 0

	nodes := make([]nodeInfo, len(allowedNodes))
	for i, nodeName := range allowedNodes {
		mem, allocMem, err := client.GetReservableMemoryBytes(ctx, nodeName, memoryAdjustment)
		if err != nil {
			return "", err
		}

		var cpus, allocCPUs int
		if cpuEnabled {
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

	requestedMemory := uint64(ptr.Deref(machine.Spec.MemoryMiB, 0)) * 1024 * 1024 // convert to bytes
	// Widen before multiplying: both fields are int32 with no upper bound, and
	// an int32 product wraps. 65536 sockets x 65536 cores lands on exactly 0
	// and 50000 x 50000 on a negative, either of which walks straight past the
	// hard-fit check below — and a negative request then subtracts from the
	// node's usage, pushing its score above 1 so it wins every time.
	requestedCPUs := int64(ptr.Deref(machine.Spec.NumSockets, 0)) * int64(ptr.Deref(machine.Spec.NumCores, 0))

	memoryTolerance := schedulerHints.GetMemoryTolerance()
	cpuTolerance := schedulerHints.GetCPUTolerance()

	type scored struct {
		nodeInfo
		scoreMem   float64
		scoreCPU   float64
		scoreTotal float64
		pending    int
	}

	wMem := float64(100-memoryTolerance) / 100.0
	wCPU := float64(100-cpuTolerance) / 100.0

	// count the existing placements per node from NodeLocations
	pendingCount := make(map[string]int, len(locations))
	for _, nl := range locations {
		pendingCount[nl.Node]++
	}

	var candidates []scored
	for _, n := range nodes {
		// Hard-fit against the raw availability reported by Proxmox.
		// Pending inflation is applied only to the score below, not here,
		// so up-to-date availability data is never double-counted.
		if requestedMemory > n.AvailableMemory {
			continue
		}
		if cpuEnabled && requestedCPUs > int64(n.AvailableCPUs) {
			continue
		}

		pending := pendingCount[n.Name]

		// With memoryAdjustment at 0 the client reports the node's whole
		// memory as both allocatable and available, which is its way of
		// saying "memory is not a constraint here". Scoring it anyway would
		// hand the decision to node size — a 400GiB host would beat a 100GiB
		// one at 0.990 to 0.960 on a resource the operator switched off — so
		// memory is left out of the score entirely, exactly as CPU is when
		// cpuAdjustment is 0.
		var scoreMem float64
		if memEnabled {
			physMem := float64(n.AllocatableMemory) * 100.0 / float64(memoryAdjustment)
			usedMem := reportedUsage(float64(n.AllocatableMemory)-float64(n.AvailableMemory)) +
				float64(pending)*float64(requestedMemory) + float64(requestedMemory)
			scoreMem = resourceScore(physMem, float64(n.AllocatableMemory), usedMem)
		}

		var scoreCPU float64
		if cpuEnabled {
			physCPU := float64(n.AllocatableCPUs) * 100.0 / float64(cpuAdjustment)
			usedCPU := reportedUsage(float64(n.AllocatableCPUs)-float64(n.AvailableCPUs)) +
				float64(pending)*float64(requestedCPUs) + float64(requestedCPUs)
			scoreCPU = resourceScore(physCPU, float64(n.AllocatableCPUs), usedCPU)
		}

		candidates = append(candidates, scored{
			nodeInfo:   n,
			scoreMem:   scoreMem,
			scoreCPU:   scoreCPU,
			scoreTotal: wMem*scoreMem + wCPU*scoreCPU,
			pending:    pending,
		})
	}

	if len(candidates) == 0 {
		return "", buildInsufficientError(nodes, requestedMemory, requestedCPUs)
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		// Primary: the curve-based score, higher wins.
		if candidates[i].scoreTotal != candidates[j].scoreTotal {
			return candidates[i].scoreTotal > candidates[j].scoreTotal
		}

		// First tie-break: raw memory headroom. Scores tie once every
		// candidate reads as saturated, and there the node with the most room
		// left is still the useful answer — it is real information the score
		// threw away when it clamped at zero.
		if candidates[i].AvailableMemory != candidates[j].AvailableMemory {
			return candidates[i].AvailableMemory > candidates[j].AvailableMemory
		}

		// Last tie-break: fewest placements already sent to the node. This is
		// what is left when the score says nothing and availability says
		// nothing either — identical hardware with both tolerances at 100, or
		// both adjustments at 0. Without it a burst would pick the same node
		// every time, since the availability above is exactly what Proxmox has
		// not caught up with yet.
		return candidates[i].pending < candidates[j].pending
	})

	decision := candidates[0].Name

	if logger := logr.FromContextOrDiscard(ctx); logger.V(4).Enabled() {
		// only construct values when message should actually be logged
		type logEntry struct {
			Node       string  `json:"node"`
			Mem        uint64  `json:"availMem"`
			CPU        int     `json:"availCpu"`
			Pending    int     `json:"pending"`
			ScoreMem   float64 `json:"scoreMem"`
			ScoreCPU   float64 `json:"scoreCpu"`
			ScoreTotal float64 `json:"scoreTotal"`
		}
		entries := make([]logEntry, len(candidates))
		for i, c := range candidates {
			entries[i] = logEntry{
				Node:       c.Name,
				Mem:        c.AvailableMemory,
				CPU:        c.AvailableCPUs,
				Pending:    c.pending,
				ScoreMem:   c.scoreMem,
				ScoreCPU:   c.scoreCPU,
				ScoreTotal: c.scoreTotal,
			}
		}
		data, _ := json.Marshal(entries)
		logger.Info("Scheduler decision",
			"candidates", string(data),
			"requestedMemory", requestedMemory,
			"requestedCPUs", requestedCPUs,
			"memoryTolerance", memoryTolerance,
			"cpuTolerance", cpuTolerance,
			"resultNode", decision,
		)
	}

	return decision, nil
}

// reportedUsage is what Proxmox says is already committed on a node, floored
// at zero: available is clamped at zero by the client, so a wildly overbooked
// node reads the same as a merely full one.
//
// The caller adds one request per placement recorded in status.NodeLocations
// on top of this. Those two quantities overlap once Proxmox has caught up
// with a clone, so a settled machine is charged twice and the overcharge
// grows with the node's history.
//
// This is deliberate, after the alternatives were measured. Taking the larger
// of the two instead of their sum stops the double charge but also stops the
// spreading: reported usage counts every guest on the host, our placements
// only ours, so on any loaded node the reported value dominates and a burst
// clumps onto one node (TestToleranceScoringSpreadsBurstOnUnequalNodes
// guards that). Dividing the score by the placement count instead penalises
// the node for its whole history, which is the same error in another form.
//
// Telling a placement that Proxmox has already reflected from one it has not
// would need a timestamp on NodeLocation, which the API does not carry. Until
// it does, the overcharge is the price of the spreading, and it is uniform
// across candidates so it shifts the scores without reordering them.
func reportedUsage(reported float64) float64 {
	if reported < 0 {
		return 0
	}

	return reported
}

// resourceScore returns the non-linear headroom score for a single resource.
// A unit of free capacity in the physical range is worth 1 point; a unit in the
// overcommit range is worth overbookDiscount points. The raw ratio is normalised
// by the capacity actually on offer and raised to saturationExponent so that
// nodes close to full are penalised disproportionately more than nodes with room
// to spare.
//
// The score always spans the full [0, 1], whatever the adjustment, so that the
// tolerances stay comparable between memory and CPU. That needs the ratio
// pinned at both ends:
//
//   - The top end: without normalising by an empty node the maximum would be
//     1 + overbookDiscount*(alloc/phys - 1), squared — 1.0 at 100% but 4.0 at
//     300%. Memory and CPU usually run at different adjustments, so summing
//     their weighted scores would silently give the more overcommitted resource
//     several times the intended weight.
//
//   - The bottom end: an adjustment below 100 caps allocation under the
//     physical size, and measuring headroom against the physical size then
//     never reaches 0 — at 50% an exhausted node still scores 0.25, at 25% it
//     scores 0.5625. The resource would vary over a fraction of the range the
//     other one uses and so weigh less than its tolerance says. Capping the
//     reference at the allocatable amount is also what underprovisioning
//     means: the operator reserved that headroom, so a node that has spent its
//     quota is full, whatever the host could still physically take.
func resourceScore(phys, alloc, used float64) float64 {
	// What the operator actually offers in the non-overcommitted range:
	// the physical size, or the lower cap an adjustment below 100 imposes.
	offered := math.Min(phys, alloc)
	if offered <= 0 {
		return 0
	}

	freeOffered := offered - used
	if freeOffered < 0 {
		freeOffered = 0
	}
	freeOver := alloc - math.Max(used, offered)
	if freeOver < 0 {
		freeOver = 0
	}

	maxFreeOver := alloc - offered

	raw := (freeOffered + overbookDiscount*freeOver) / (offered + overbookDiscount*maxFreeOver)

	return math.Pow(raw, saturationExponent)
}

// buildInsufficientError returns the best-fitting error when no candidate fits
// the request: InsufficientMemoryError if no node has enough memory available,
// otherwise InsufficientCPUError pointing at the node with the largest CPU headroom.
func buildInsufficientError(nodes []nodeInfo, requestedMemory uint64, requestedCPUs int64) error {
	// Callers gate on a non-empty allowed-node list, so this is unreachable
	// today; indexing into the empty slice below would panic if that ever
	// stopped holding.
	if len(nodes) == 0 {
		return errors.New("no node to schedule on: the allowed node list is empty")
	}

	byMem := make([]nodeInfo, len(nodes))
	copy(byMem, nodes)
	sort.Sort(sortByAvailableMemory(byMem))
	if requestedMemory > byMem[0].AvailableMemory {
		return InsufficientMemoryError{
			node:      byMem[0].Name,
			available: byMem[0].AvailableMemory,
			requested: requestedMemory,
		}
	}
	bestCPU := nodes[0]
	for _, n := range nodes[1:] {
		if n.AvailableCPUs > bestCPU.AvailableCPUs {
			bestCPU = n
		}
	}
	return InsufficientCPUError{
		node:      bestCPU.Name,
		available: bestCPU.AvailableCPUs,
		requested: int(requestedCPUs),
	}
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
