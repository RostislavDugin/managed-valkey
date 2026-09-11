package valkey

import (
	"context"
	"errors"
	"testing"
	"time"
)

func Test_CanPlaceReservations_WithExactNodeBoundary_AcceptsHA(t *testing.T) {
	topology := ClusterTopology{NodeCount: 3, NodeCPUMilli: 1000, NodeRAMMiB: 4096}

	placed, err := canPlaceReservations(context.Background(), topology, []placementGroup{
		{processes: 3, cpuMilli: 1000, ramMiB: 4096},
	})

	if err != nil || !placed {
		t.Fatalf("размещение=%t, ошибка=%v", placed, err)
	}
}

func Test_CanPlaceReservations_WhenClusterIsFragmented_RejectsSingle(t *testing.T) {
	topology := ClusterTopology{NodeCount: 3, NodeCPUMilli: 3000, NodeRAMMiB: 12288}
	groups := []placementGroup{
		{processes: 1, cpuMilli: 2000, ramMiB: 8192},
		{processes: 1, cpuMilli: 2000, ramMiB: 8192},
		{processes: 1, cpuMilli: 2000, ramMiB: 8192},
		{processes: 1, cpuMilli: 2000, ramMiB: 8192},
	}

	placed, err := canPlaceReservations(context.Background(), topology, groups)

	if err != nil || placed {
		t.Fatalf("размещение=%t, ошибка=%v", placed, err)
	}
}

func Test_CanPlaceReservations_WithTwoNodes_RejectsHA(t *testing.T) {
	topology := ClusterTopology{NodeCount: 2, NodeCPUMilli: 4000, NodeRAMMiB: 16384}

	placed, err := canPlaceReservations(context.Background(), topology, []placementGroup{
		{processes: 3, cpuMilli: 1000, ramMiB: 1024},
	})

	if err != nil || placed {
		t.Fatalf("размещение=%t, ошибка=%v", placed, err)
	}
}

func Test_CanPlaceReservations_WithMixedSingleAndHA_UsesBothResourcesAndDistinctNodes(t *testing.T) {
	tests := []struct {
		name     string
		topology ClusterTopology
		groups   []placementGroup
		want     bool
	}{
		{
			name: "смешанные резервы помещаются",
			topology: ClusterTopology{
				NodeCount: 3, NodeCPUMilli: 4000, NodeRAMMiB: 8192,
			},
			groups: []placementGroup{
				{processes: 3, cpuMilli: 1000, ramMiB: 2048},
				{processes: 1, cpuMilli: 2000, ramMiB: 4096},
			},
			want: true,
		},
		{
			name: "CPU и RAM по отдельности помещаются, но совместной раскладки нет",
			topology: ClusterTopology{
				NodeCount: 2, NodeCPUMilli: 10000, NodeRAMMiB: 10240,
			},
			groups: []placementGroup{
				{processes: 1, cpuMilli: 8000, ramMiB: 8192},
				{processes: 1, cpuMilli: 2000, ramMiB: 6144},
				{processes: 1, cpuMilli: 6000, ramMiB: 2048},
				{processes: 1, cpuMilli: 4000, ramMiB: 4096},
			},
			want: false,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			placed, err := canPlaceReservations(context.Background(), testCase.topology, testCase.groups)
			if err != nil || placed != testCase.want {
				t.Fatalf("размещение=%t, ожидалось=%t, ошибка=%v", placed, testCase.want, err)
			}
		})
	}
}

func Test_CanPlaceReservations_WhenGreedyOrderCannotPlaceAll_FindsAnotherAssignment(t *testing.T) {
	topology := ClusterTopology{NodeCount: 2, NodeCPUMilli: 10000, NodeRAMMiB: 10240}
	groups := []placementGroup{
		{processes: 1, cpuMilli: 6000, ramMiB: 1024},
		{processes: 1, cpuMilli: 5000, ramMiB: 1024},
		{processes: 1, cpuMilli: 3000, ramMiB: 1024},
		{processes: 1, cpuMilli: 2000, ramMiB: 1024},
		{processes: 1, cpuMilli: 2000, ramMiB: 1024},
		{processes: 1, cpuMilli: 2000, ramMiB: 1024},
	}

	placed, err := canPlaceReservations(context.Background(), topology, groups)

	if err != nil || !placed {
		t.Fatalf("размещение=%t, ошибка=%v", placed, err)
	}
}

func Test_CanPlaceReservations_OnSmallSets_MatchesIndependentExhaustiveSearch(t *testing.T) {
	topology := ClusterTopology{NodeCount: 3, NodeCPUMilli: 4000, NodeRAMMiB: 4096}
	variants := []placementGroup{
		{processes: 1, cpuMilli: 1000, ramMiB: 1024},
		{processes: 1, cpuMilli: 3000, ramMiB: 1024},
		{processes: 1, cpuMilli: 1000, ramMiB: 3072},
		{processes: 3, cpuMilli: 1000, ramMiB: 1024},
	}

	for first := range variants {
		for second := range variants {
			for third := range variants {
				groups := []placementGroup{variants[first], variants[second], variants[third]}
				want := exhaustivePlacement(topology, groups)
				placed, err := canPlaceReservations(context.Background(), topology, groups)
				if err != nil || placed != want {
					t.Fatalf(
						"набор=%d/%d/%d, размещение=%t, ожидалось=%t, ошибка=%v",
						first,
						second,
						third,
						placed,
						want,
						err,
					)
				}
			}
		}
	}
}

func Test_CanPlaceReservations_WithThirtyTwoInstances_CompletesBeforeDeadline(t *testing.T) {
	topology := ClusterTopology{NodeCount: 3, NodeCPUMilli: 32000, NodeRAMMiB: 131072}
	groups := thirtyTwoPlacementGroups()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	placed, err := canPlaceReservations(ctx, topology, groups)

	if err != nil || !placed {
		t.Fatalf("размещение=%t, ошибка=%v", placed, err)
	}
}

func BenchmarkCanPlaceReservationsWithThirtyTwoInstances(b *testing.B) {
	topology := ClusterTopology{NodeCount: 3, NodeCPUMilli: 32000, NodeRAMMiB: 131072}
	groups := thirtyTwoPlacementGroups()

	for b.Loop() {
		placed, err := canPlaceReservations(context.Background(), topology, groups)
		if err != nil || !placed {
			b.Fatalf("размещение=%t, ошибка=%v", placed, err)
		}
	}
}

func thirtyTwoPlacementGroups() []placementGroup {
	groups := make([]placementGroup, 0, 32)
	for index := range 32 {
		groups = append(groups, placementGroup{
			processes: 1,
			cpuMilli:  int64(index%4+1) * 1000,
			ramMiB:    int64(index%4+1) * 1024,
		})
	}

	return groups
}

func Test_CanPlaceReservations_WhenContextIsCanceled_StopsSearch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	placed, err := canPlaceReservations(ctx, ClusterTopology{
		NodeCount: 3, NodeCPUMilli: 4000, NodeRAMMiB: 16384,
	}, []placementGroup{{processes: 1, cpuMilli: 1000, ramMiB: 1024}})

	if placed || !errors.Is(err, context.Canceled) {
		t.Fatalf("размещение=%t, ошибка=%v", placed, err)
	}
}

func exhaustivePlacement(topology ClusterTopology, groups []placementGroup) bool {
	nodes := make([]nodeBudget, topology.NodeCount)
	for index := range nodes {
		nodes[index] = nodeBudget{cpuMilli: topology.NodeCPUMilli, ramMiB: topology.NodeRAMMiB}
	}

	var place func(int, int, uint64) bool
	place = func(groupIndex, processIndex int, usedNodes uint64) bool {
		if groupIndex == len(groups) {
			return true
		}
		group := groups[groupIndex]
		if processIndex == group.processes {
			return place(groupIndex+1, 0, 0)
		}
		for nodeIndex := range nodes {
			mask := uint64(1) << nodeIndex
			if usedNodes&mask != 0 ||
				nodes[nodeIndex].cpuMilli < group.cpuMilli ||
				nodes[nodeIndex].ramMiB < group.ramMiB {
				continue
			}
			nodes[nodeIndex].cpuMilli -= group.cpuMilli
			nodes[nodeIndex].ramMiB -= group.ramMiB
			if place(groupIndex, processIndex+1, usedNodes|mask) {
				return true
			}
			nodes[nodeIndex].cpuMilli += group.cpuMilli
			nodes[nodeIndex].ramMiB += group.ramMiB
		}

		return false
	}

	return place(0, 0, 0)
}
