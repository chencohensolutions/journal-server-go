//go:build !unix && !windows

package media

import "math"

// freeBytes is unknown on this platform, so uploads are never refused for space.
func freeBytes(string) (uint64, error) { return math.MaxUint64, nil }
