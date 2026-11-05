package version

import (
	"regexp"
	"strings"
	"testing"
)

func TestNumberIsSemantic(t *testing.T) {
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(Number) {
		t.Fatalf("version/VERSION is %q, not major.minor.patch", Number)
	}
	if !strings.HasPrefix(String(), Number) {
		t.Errorf("String() = %q, doesn't start with %s", String(), Number)
	}
}
