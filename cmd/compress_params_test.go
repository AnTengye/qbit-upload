package cmd

import (
	"math"
	"testing"
)

func TestCalcSevenZipParams(t *testing.T) {
	tests := []struct {
		name          string
		availMB       uint64
		reserveMB     int64
		cpuCores      int
		wantDictMaxMB int // result dict must be <= this
		wantDictMinMB int // result dict must be >= this
		wantThreadMax int
		wantThreadMin int
		// fitsInBudget: true means the result must satisfy the memory constraint
		checkBudget bool
	}{
		{
			name:          "unknown memory (availMB=0) uses CPU defaults",
			availMB:       0,
			reserveMB:     2048,
			cpuCores:      8,
			wantDictMaxMB: 64,
			wantDictMinMB: 64,
			wantThreadMax: 6, // 8 - 2
			wantThreadMin: 6,
			checkBudget:   false,
		},
		{
			name:          "single core CPU falls back to 1 thread",
			availMB:       0,
			reserveMB:     2048,
			cpuCores:      1,
			wantDictMaxMB: 64,
			wantDictMinMB: 64,
			wantThreadMax: 1,
			wantThreadMin: 1,
			checkBudget:   false,
		},
		{
			name:          "generous memory (8GB avail, 2GB reserve) → large dict",
			availMB:       8192,
			reserveMB:     2048,
			cpuCores:      8,
			wantDictMaxMB: 256,
			wantDictMinMB: 64,
			wantThreadMax: 6,
			wantThreadMin: 1,
			checkBudget:   true,
		},
		{
			// budget = 4096 - 2048 = 2048 MB; 128m × 1 thread ≈ 1472 MB fits → dict=128 is valid
			name:          "moderate memory (4GB avail, 2GB reserve, 8 cores)",
			availMB:       4096,
			reserveMB:     2048,
			cpuCores:      8,
			wantDictMaxMB: 128,
			wantDictMinMB: 16,
			wantThreadMax: 6,
			wantThreadMin: 1,
			checkBudget:   true,
		},
		{
			// budget = 6144 - 2048 = 4096 MB; 256m × 1 thread ≈ 2944 MB fits → dict=256 is valid
			name:          "previously OOM scenario (6GB avail, 2GB reserve, 8 cores)",
			availMB:       6144,
			reserveMB:     2048,
			cpuCores:      8,
			wantDictMaxMB: 256,
			wantDictMinMB: 1,
			wantThreadMax: 6,
			wantThreadMin: 1,
			checkBudget:   true,
		},
		{
			name:          "very low memory budget (512MB avail, 256MB reserve)",
			availMB:       512,
			reserveMB:     256,
			cpuCores:      4,
			wantDictMaxMB: 16,
			wantDictMinMB: 1,
			wantThreadMax: 2,
			wantThreadMin: 1,
			checkBudget:   true,
		},
		{
			name:          "reserve exceeds available memory → minimal fallback",
			availMB:       1024,
			reserveMB:     4096,
			cpuCores:      4,
			wantDictMaxMB: 1,
			wantDictMinMB: 1,
			wantThreadMax: 1,
			wantThreadMin: 1,
			checkBudget:   false, // budget is 0, still must not panic
		},
		{
			name:          "exact budget boundary: 64m × 1 thread = 736MB, avail=2784 reserve=2048",
			availMB:       2784,
			reserveMB:     2048,
			cpuCores:      4,
			wantDictMaxMB: 64,
			wantDictMinMB: 1,
			wantThreadMax: 2,
			wantThreadMin: 1,
			checkBudget:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dict, threads := calcSevenZipParams(tc.availMB, tc.reserveMB, tc.cpuCores)

			if dict < tc.wantDictMinMB || dict > tc.wantDictMaxMB {
				t.Errorf("dictSizeMB = %d, want [%d, %d]", dict, tc.wantDictMinMB, tc.wantDictMaxMB)
			}
			if threads < tc.wantThreadMin || threads > tc.wantThreadMax {
				t.Errorf("threads = %d, want [%d, %d]", threads, tc.wantThreadMin, tc.wantThreadMax)
			}
			if dict < 1 {
				t.Errorf("dictSizeMB must be >= 1, got %d", dict)
			}
			if threads < 1 {
				t.Errorf("threads must be >= 1, got %d", threads)
			}

			// Verify dict is a power of two.
			if dict&(dict-1) != 0 {
				t.Errorf("dictSizeMB %d is not a power of two", dict)
			}

			// Verify memory budget constraint is satisfied.
			if tc.checkBudget && tc.availMB > 0 {
				var budgetMB int
				if tc.availMB > uint64(tc.reserveMB) {
					budgetMB = int(tc.availMB - uint64(tc.reserveMB))
				}
				required := float64(dict) * memCoeffPerThread * float64(threads)
				if required > math.Ceil(float64(budgetMB)) {
					t.Errorf("memory constraint violated: required %.1f MiB > budget %d MiB (dict=%d, threads=%d)",
						required, budgetMB, dict, threads)
				}
			}
		})
	}
}
