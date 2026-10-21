package media

import (
	"fmt"
	"os"
	"path"
	"strings"
	"time"
	"unicode/utf8"
)

// IsSha256 reports whether s is a sha256 as the API uses it: 64 lowercase hex characters.
func IsSha256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// isMimeType reports whether s is a plain "type/subtype" media type, as it is sent back in Content-Type.
func isMimeType(s string) bool {
	kind, subtype, ok := strings.Cut(s, "/")
	if !ok || kind == "" || subtype == "" || len(s) > 127 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("/.+-_", c)) {
			return false
		}
	}
	return true
}

// monthDir is media/YYYY/YYYY-MM for the photo's own local time (its offset is kept in takenAt),
// so a photo is where you would look for it; files without a date go to media/undated.
func monthDir(takenAt *time.Time) string {
	if takenAt == nil {
		return "media/undated"
	}
	return path.Join("media", takenAt.Format("2006"), takenAt.Format("2006-01"))
}

// Names Windows refuses as file names, with any extension.
var reservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

const (
	maxStemRunes = 100
	maxExtRunes  = 16
)

// safeFileName turns the name the device sent into one that is valid on Windows, macOS and Linux:
// no folders, no reserved characters or names, no trailing dots or spaces, and a bounded length.
// Square brackets go too: libvips reads a trailing "[...]" in a file name as loading options.
func safeFileName(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`<>:"/\|?*[]`, r) || r == utf8.RuneError {
			return '_'
		}
		return r
	}, name)
	name = strings.TrimLeft(strings.TrimRight(name, ". "), " ")

	stem, ext := name, ""
	if i := strings.LastIndex(name, "."); i > 0 {
		stem, ext = name[:i], name[i:]
	}
	stem = strings.TrimRight(truncateRunes(stem, maxStemRunes), ". ")
	ext = truncateRunes(ext, maxExtRunes)
	if stem == "" {
		stem = "file"
	}
	if reservedNames[strings.ToUpper(strings.SplitN(stem, ".", 2)[0])] {
		stem = "_" + stem
	}
	return stem + ext
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// freeName picks name, or "name (2).ext", "name (3).ext", … so that neither the file nor its sidecar
// matches an entry already in dir, ignoring case, since the folder must also work on Windows and macOS.
func freeName(dir string, name string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	taken := make(map[string]bool, len(entries))
	for _, entry := range entries {
		taken[strings.ToLower(entry.Name())] = true
	}
	stem, ext := name, ""
	if i := strings.LastIndex(name, "."); i > 0 {
		stem, ext = name[:i], name[i:]
	}
	for n := 1; ; n++ {
		candidate := name
		if n > 1 {
			candidate = fmt.Sprintf("%s (%d)%s", stem, n, ext)
		}
		lower := strings.ToLower(candidate)
		if !taken[lower] && !taken[lower+".json"] {
			return candidate, nil
		}
	}
}
