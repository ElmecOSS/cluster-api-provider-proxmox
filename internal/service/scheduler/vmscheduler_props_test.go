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

// Properties the scoring algorithm has to hold for every input, checked by
// sweeping the space rather than by picking examples. The example-based tests
// next door pin specific decisions; these pin the shape of the function those
// decisions come out of, and catch a regression wherever it is introduced.

import (
	"context"
	"math"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"

	infrav1 "github.com/ionos-cloud/cluster-api-provider-proxmox/api/v1alpha2"
)

// A score outside [0, 1] would break the weighting: the tolerances are applied
// as plain multipliers, so a resource whose score can exceed 1 outweighs the
// other one by however much it overshoots. NaN would be worse still — it
// compares false against everything and quietly sinks a node to last place.
func TestPropScoreStaysWithinBounds(t *testing.T) {
	r := rand.New(rand.NewSource(1)) //nolint:gosec // a fixed seed is the point: a failure has to be reproducible
	for range 1_000_000 {
		phys, alloc, used := r.Float64()*1024, r.Float64()*4096, r.Float64()*8192
		s := resourceScore(phys, alloc, used)
		if math.IsNaN(s) || math.IsInf(s, 0) || s < 0 || s > 1 {
			t.Fatalf("resourceScore(%v, %v, %v) = %v, want a value in [0, 1]", phys, alloc, used, s)
		}
	}
}

// Filling a node must never improve its score, whichever range the usage falls
// in — physical, overcommit, or past the cap.
func TestPropScoreNeverRewardsMoreUsage(t *testing.T) {
	r := rand.New(rand.NewSource(2)) //nolint:gosec // a fixed seed is the point: a failure has to be reproducible
	for range 500_000 {
		phys := 1 + r.Float64()*1024
		alloc := phys * (0.1 + r.Float64()*4)
		less := r.Float64() * alloc
		more := less + r.Float64()*alloc
		require.GreaterOrEqual(t,
			resourceScore(phys, alloc, less), resourceScore(phys, alloc, more),
			"phys=%v alloc=%v: %v used scored below %v used", phys, alloc, less, more)
	}
}

// The score has to span the whole range at every adjustment: an empty node is
// 1, a node that has handed out everything it may is 0. Both ends matter.
// Without the top one an overcommitted resource peaks above 1; without the
// bottom one an adjustment below 100 never reaches 0, so that resource varies
// over a fraction of the span the other uses and weighs less than its
// tolerance says.
func TestPropScoreSpansTheFullRangeAtEveryAdjustment(t *testing.T) {
	for _, adj := range []float64{10, 25, 50, 95, 100, 150, 300, 1000} {
		for _, phys := range []float64{1, 24, 32, 64, 512} {
			alloc := phys * adj / 100
			require.InDelta(t, 1.0, resourceScore(phys, alloc, 0), 1e-12,
				"empty node at adjustment %v, %v physical", adj, phys)
			require.InDelta(t, 0.0, resourceScore(phys, alloc, alloc), 1e-12,
				"exhausted node at adjustment %v, %v physical", adj, phys)
		}
	}
}

// Whatever it is handed, selectNode either returns a node the request actually
// fits on or an error — never a node that cannot take the VM, and never a
// panic.
func TestPropSelectNodeOnlyReturnsNodesThatFit(t *testing.T) {
	r := rand.New(rand.NewSource(4)) //nolint:gosec // a fixed seed is the point: a failure has to be reproducible
	var placed int

	for range 20_000 {
		n := 1 + r.Intn(5)
		names := make([]string, n)
		mem, memTotal := map[string]uint64{}, map[string]uint64{}
		cpus, cpusTotal := map[string]int{}, map[string]int{}
		for j := range n {
			names[j] = string(rune('a' + j))
			memTotal[names[j]] = uint64(1<<34) + uint64(r.Intn(1<<40))
			mem[names[j]] = uint64(r.Int63n(int64(memTotal[names[j]]) + 1))
			cpusTotal[names[j]] = 1 + r.Intn(256)
			cpus[names[j]] = r.Intn(cpusTotal[names[j]] + 1)
		}

		placements := r.Intn(10)
		locations := make([]infrav1.NodeLocation, 0, placements)
		for range placements {
			locations = append(locations, infrav1.NodeLocation{Node: names[r.Intn(n)]})
		}

		machine := &infrav1.ProxmoxMachine{Spec: infrav1.ProxmoxMachineSpec{
			MemoryMiB:  ptr.To(int32(1 + r.Intn(1<<14))),
			NumSockets: ptr.To(int32(1 + r.Intn(2))),
			NumCores:   ptr.To(int32(1 + r.Intn(32))),
		}}
		hints := &infrav1.SchedulerHints{
			MemoryAdjustment: ptr.To(int64(r.Intn(400))),
			CPUAdjustment:    ptr.To(int64(r.Intn(400))),
			MemoryTolerance:  ptr.To(int64(r.Intn(101))),
			CPUTolerance:     ptr.To(int64(r.Intn(101))),
		}

		cl := fakeResourceClient{memory: mem, memoryTotal: memTotal, cpus: cpus, cpusTotal: cpusTotal}
		node, err := selectNode(context.Background(), cl, machine, locations, names, hints)
		if err != nil {
			continue
		}
		placed++

		require.LessOrEqual(t, uint64(*machine.Spec.MemoryMiB)*1024*1024, mem[node],
			"picked %q without the memory to hold the VM", node)
		if *hints.CPUAdjustment > 0 {
			require.LessOrEqual(t, int(*machine.Spec.NumSockets)*int(*machine.Spec.NumCores), cpus[node],
				"picked %q without the cores to hold the VM", node)
		}
	}

	require.Greater(t, placed, 10_000, "too few placements to call this a sweep")
}

// Mirroring the problem — memory and CPU swapped on every node, and their
// adjustments and tolerances swapped with them — has to pick the same node.
// Otherwise one resource carries more weight than the other for a structural
// reason rather than because the operator asked for it.
//
// This says nothing about the scale of the score, which a uniform distortion
// would preserve on both sides of the mirror; that is what the span property
// above is for. What it does catch is a term that applies to only one of the
// two resources.
//
// Memory is expressed as GiB per core so the swap is well defined. Ties are
// skipped: the last tie-break reads free memory and is asymmetric on purpose.
func TestPropNeitherResourceIsStructurallyFavoured(t *testing.T) {
	const giB = uint64(1) << 30
	r := rand.New(rand.NewSource(5)) //nolint:gosec // a fixed seed is the point: a failure has to be reproducible
	var compared int

	toBytes := func(in map[string]int) map[string]uint64 {
		out := make(map[string]uint64, len(in))
		for k, v := range in {
			out[k] = uint64(v) * giB
		}
		return out
	}

	for range 20_000 {
		n := 2 + r.Intn(3)
		names := make([]string, n)
		memUnits, memTotalUnits := map[string]int{}, map[string]int{}
		cpus, cpusTotal := map[string]int{}, map[string]int{}
		for j := range n {
			names[j] = string(rune('a' + j))
			memTotalUnits[names[j]] = 1 + r.Intn(256)
			memUnits[names[j]] = r.Intn(memTotalUnits[names[j]] + 1)
			cpusTotal[names[j]] = 1 + r.Intn(256)
			cpus[names[j]] = r.Intn(cpusTotal[names[j]] + 1)
		}

		memAdj, cpuAdj := int64(1+r.Intn(400)), int64(1+r.Intn(400))
		memTol, cpuTol := int64(r.Intn(101)), int64(r.Intn(101))
		request := int32(1 + r.Intn(16))

		machine := &infrav1.ProxmoxMachine{Spec: infrav1.ProxmoxMachineSpec{
			MemoryMiB:  ptr.To(request * 1024),
			NumSockets: ptr.To(int32(1)),
			NumCores:   ptr.To(request),
		}}

		straight, err := selectNode(context.Background(),
			fakeResourceClient{
				memory: toBytes(memUnits), memoryTotal: toBytes(memTotalUnits),
				cpus: cpus, cpusTotal: cpusTotal,
			},
			machine, nil, names, &infrav1.SchedulerHints{
				MemoryAdjustment: &memAdj, CPUAdjustment: &cpuAdj,
				MemoryTolerance: &memTol, CPUTolerance: &cpuTol,
			})

		mirrored, mirrorErr := selectNode(context.Background(),
			fakeResourceClient{
				memory: toBytes(cpus), memoryTotal: toBytes(cpusTotal),
				cpus: memUnits, cpusTotal: memTotalUnits,
			},
			machine, nil, names, &infrav1.SchedulerHints{
				MemoryAdjustment: &cpuAdj, CPUAdjustment: &memAdj,
				MemoryTolerance: &cpuTol, CPUTolerance: &memTol,
			})

		if err != nil || mirrorErr != nil {
			require.Error(t, err, "the mirrored problem did not fit but the original did")
			require.Error(t, mirrorErr, "the original did not fit but the mirrored problem did")
			continue
		}

		compared++
		require.Equal(t, straight, mirrored,
			"swapping the two resources changed the winner (memAdjustment=%d cpuAdjustment=%d memTolerance=%d cpuTolerance=%d)",
			memAdj, cpuAdj, memTol, cpuTol)
	}

	require.Greater(t, compared, 10_000, "too few comparable pairs to call this a sweep")
}
