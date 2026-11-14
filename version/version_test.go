package version

import (
	"regexp"
	"strings"
	"testing"
)

// A version is the day it was made, YYYY.MM.DD, with .2, .3, ... for another the same day (docs/install.md
// "Versions"); older ones were major.minor.patch, which reads the same way.
func TestNumberIsADateVersion(t *testing.T) {
	if !regexp.MustCompile(`^\d+\.\d+\.\d+(\.\d+)?$`).MatchString(Number) {
		t.Fatalf("version/VERSION is %q, not YYYY.MM.DD or YYYY.MM.DD.N", Number)
	}
	if !strings.HasPrefix(String(), Number) {
		t.Errorf("String() = %q, doesn't start with %s", String(), Number)
	}
}
