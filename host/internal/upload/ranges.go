package upload

import "sort"

// ComputeContiguousRanges converts a slice of sequence numbers into contiguous ranges [start, end].
// E.g. [0, 1, 2, 5, 6, 8] -> [[0, 2], [5, 6], [8, 8]]
func ComputeContiguousRanges(seqs []int) [][2]int {
	if len(seqs) == 0 {
		return [][2]int{}
	}

	// Ensure sorted and unique
	sorted := make([]int, len(seqs))
	copy(sorted, seqs)
	sort.Ints(sorted)

	var ranges [][2]int
	start := sorted[0]
	prev := sorted[0]

	for i := 1; i < len(sorted); i++ {
		curr := sorted[i]
		if curr == prev {
			continue // skip duplicates
		}
		if curr == prev+1 {
			prev = curr
		} else {
			ranges = append(ranges, [2]int{start, prev})
			start = curr
			prev = curr
		}
	}
	ranges = append(ranges, [2]int{start, prev})
	return ranges
}

// IsContiguousUpTo checks if all sequence numbers from 0 to lastSeq exist in the slice.
func IsContiguousUpTo(seqs []int, lastSeq int) bool {
	if lastSeq < 0 {
		return false
	}
	have := make(map[int]bool, len(seqs))
	for _, s := range seqs {
		have[s] = true
	}
	for i := 0; i <= lastSeq; i++ {
		if !have[i] {
			return false
		}
	}
	return true
}
