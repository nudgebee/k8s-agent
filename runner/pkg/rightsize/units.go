package rightsize

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

// parseCPUCores parses a Kubernetes CPU quantity string ("250m", "0.5", "2")
// into cores. Mirrors the legacy PodResources.parse_cpu; returns 0 on empty /
// unparseable input (the original was equally defensive).
func parseCPUCores(cpu string) float64 {
	cpu = strings.TrimSpace(cpu)
	if cpu == "" {
		return 0
	}
	q, err := resource.ParseQuantity(cpu)
	if err != nil {
		return 0
	}
	return q.AsApproximateFloat64()
}

// parseMemBytes parses a Kubernetes memory quantity ("100Mi", "1Gi", "1000000")
// into bytes. Returns 0 on empty / unparseable input.
func parseMemBytes(mem string) float64 {
	mem = strings.TrimSpace(mem)
	if mem == "" {
		return 0
	}
	q, err := resource.ParseQuantity(mem)
	if err != nil {
		return 0
	}
	return q.AsApproximateFloat64()
}

// formatAndRoundUnit renders a resource amount the way the recommendation is
// applied to the workload, mirroring the legacy format_and_round_unit:
//
//   - x < 1            → millicpu, e.g. 0.05 → "50m"
//   - 1 <= x < 500     → bare CPU cores, e.g. 2 → "2"
//   - x >= 500         → binary memory unit (Ki/Mi/Gi/…), value rounded UP to
//     the nearest 10 in the chosen unit, e.g. 104857600 → "100Mi"
//
// The >=500 branch assumes a memory byte count (no workload requests 500 CPUs,
// and 500 bytes of memory is never a real allocation) — the same heuristic the
// original relied on.
func formatAndRoundUnit(x float64) string {
	const base = 1024.0
	if x < 1 {
		return fmt.Sprintf("%dm", int(x*1000))
	}
	if x < 500 { // CPU cores
		return strconv.FormatFloat(x, 'g', -1, 64)
	}

	units := []string{"", "Ki", "Mi", "Gi", "Ti", "Pi", "Ei"}
	xi := math.Floor(x)
	for i, unit := range units {
		nextPow := math.Pow(base, float64(i+1))
		if xi < nextPow || i == len(units)-1 || xi/nextPow < 10 {
			value := xi / math.Pow(base, float64(i))
			value = math.Ceil(value/10) * 10 // round up to nearest 10
			return fmt.Sprintf("%.0f%s", value, unit)
		}
	}
	return fmt.Sprintf("%d", int(xi))
}
