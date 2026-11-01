package cli

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// parseCacheByteLimit uses integer arithmetic so a rounded or overflowing
// size can never silently turn into a more aggressive pruning policy.
func parseCacheByteLimit(value string) (int64, error) {
	value = strings.TrimSpace(value)
	end := 0
	for end < len(value) && value[end] >= '0' && value[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, fmt.Errorf("size must be a non-negative whole number, optionally followed by B, KB, MB, GB, TB, KiB, MiB, GiB, or TiB")
	}
	amount, err := strconv.ParseUint(value[:end], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid byte count: %w", err)
	}
	unit := strings.ToUpper(strings.TrimSpace(value[end:]))
	multipliers := map[string]uint64{
		"": 1, "B": 1,
		"KB": 1000, "MB": 1000 * 1000, "GB": 1000 * 1000 * 1000, "TB": 1000 * 1000 * 1000 * 1000,
		"KIB": 1 << 10, "MIB": 1 << 20, "GIB": 1 << 30, "TIB": 1 << 40,
	}
	multiplier, ok := multipliers[unit]
	if !ok {
		return 0, fmt.Errorf("unsupported size unit %q; use B, KB, MB, GB, TB, KiB, MiB, GiB, or TiB", value[end:])
	}
	if amount > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("size exceeds %d bytes", int64(math.MaxInt64))
	}
	return int64(amount * multiplier), nil
}
