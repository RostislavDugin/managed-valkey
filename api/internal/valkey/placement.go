package valkey

import (
	"context"
	"sort"
	"strconv"
)

type placementGroup struct {
	processes int
	cpuMilli  int64
	ramMiB    int64
}

type nodeBudget struct {
	cpuMilli int64
	ramMiB   int64
}

func canPlaceReservations(
	ctx context.Context,
	topology ClusterTopology,
	groups []placementGroup,
) (bool, error) {
	ordered := append([]placementGroup(nil), groups...)
	sort.SliceStable(ordered, func(left, right int) bool {
		if ordered[left].processes != ordered[right].processes {
			return ordered[left].processes > ordered[right].processes
		}
		leftConstraint := max(
			float64(ordered[left].cpuMilli)/float64(topology.NodeCPUMilli),
			float64(ordered[left].ramMiB)/float64(topology.NodeRAMMiB),
		)
		rightConstraint := max(
			float64(ordered[right].cpuMilli)/float64(topology.NodeCPUMilli),
			float64(ordered[right].ramMiB)/float64(topology.NodeRAMMiB),
		)
		if leftConstraint != rightConstraint {
			return leftConstraint > rightConstraint
		}
		if ordered[left].cpuMilli != ordered[right].cpuMilli {
			return ordered[left].cpuMilli > ordered[right].cpuMilli
		}

		return ordered[left].ramMiB > ordered[right].ramMiB
	})

	var totalCPUMilli, totalRAMMiB int64
	for _, group := range ordered {
		if group.processes > topology.NodeCount ||
			group.cpuMilli > topology.NodeCPUMilli ||
			group.ramMiB > topology.NodeRAMMiB {
			return false, nil
		}
		totalCPUMilli += int64(group.processes) * group.cpuMilli
		totalRAMMiB += int64(group.processes) * group.ramMiB
	}
	if totalCPUMilli > int64(topology.NodeCount)*topology.NodeCPUMilli ||
		totalRAMMiB > int64(topology.NodeCount)*topology.NodeRAMMiB {
		return false, nil
	}

	nodes := make([]nodeBudget, topology.NodeCount)
	for index := range nodes {
		nodes[index] = nodeBudget{cpuMilli: topology.NodeCPUMilli, ramMiB: topology.NodeRAMMiB}
	}
	memo := make(map[string]struct{})

	return searchPlacement(ctx, ordered, 0, nodes, memo)
}

func searchPlacement(
	ctx context.Context,
	groups []placementGroup,
	groupIndex int,
	nodes []nodeBudget,
	memo map[string]struct{},
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if groupIndex == len(groups) {
		return true, nil
	}

	state := placementStateKey(groupIndex, nodes)
	if _, found := memo[state]; found {
		return false, nil
	}

	group := groups[groupIndex]
	found, err := searchGroupAssignments(ctx, nodes, group, 0, nil, func(assigned []nodeBudget) (bool, error) {
		sortNodeBudgets(assigned)

		return searchPlacement(ctx, groups, groupIndex+1, assigned, memo)
	})
	if err != nil {
		return false, err
	}
	if found {
		return true, nil
	}

	memo[state] = struct{}{}

	return false, nil
}

func searchGroupAssignments(
	ctx context.Context,
	nodes []nodeBudget,
	group placementGroup,
	start int,
	selected []int,
	next func([]nodeBudget) (bool, error),
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if len(selected) == group.processes {
		assigned := append([]nodeBudget(nil), nodes...)
		for _, nodeIndex := range selected {
			assigned[nodeIndex].cpuMilli -= group.cpuMilli
			assigned[nodeIndex].ramMiB -= group.ramMiB
		}

		return next(assigned)
	}

	remaining := group.processes - len(selected)
	seen := make(map[nodeBudget]struct{})
	for nodeIndex := start; nodeIndex <= len(nodes)-remaining; nodeIndex++ {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		node := nodes[nodeIndex]
		if node.cpuMilli < group.cpuMilli || node.ramMiB < group.ramMiB {
			continue
		}
		if _, duplicate := seen[node]; duplicate {
			continue
		}
		seen[node] = struct{}{}

		selected = append(selected, nodeIndex)
		found, err := searchGroupAssignments(ctx, nodes, group, nodeIndex+1, selected, next)
		selected = selected[:len(selected)-1]
		if err != nil || found {
			return found, err
		}
	}

	return false, nil
}

func sortNodeBudgets(nodes []nodeBudget) {
	sort.Slice(nodes, func(left, right int) bool {
		if nodes[left].cpuMilli != nodes[right].cpuMilli {
			return nodes[left].cpuMilli < nodes[right].cpuMilli
		}

		return nodes[left].ramMiB < nodes[right].ramMiB
	})
}

func placementStateKey(groupIndex int, nodes []nodeBudget) string {
	buffer := strconv.AppendInt(nil, int64(groupIndex), 10)
	for _, node := range nodes {
		buffer = append(buffer, ':')
		buffer = strconv.AppendInt(buffer, node.cpuMilli, 10)
		buffer = append(buffer, '/')
		buffer = strconv.AppendInt(buffer, node.ramMiB, 10)
	}

	return string(buffer)
}
