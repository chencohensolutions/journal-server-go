//go:build !unix && !windows

package copies

// sameDisk is unknown on this platform, so it is never warned about.
func sameDisk(string, string) (bool, error) { return false, nil }
