package media

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// MaxAppDataSize is the largest app data SetAppData keeps, in bytes once compacted.
const MaxAppDataSize = 1 << 20

// ErrAppDataName, ErrAppDataNotObject and ErrAppDataTooLarge are SetAppData's answers to what it won't keep.
var (
	ErrAppDataName      = errors.New("an app data name is 1–40 lower-case letters, digits or dashes")
	ErrAppDataNotObject = errors.New("app data is a JSON object")
	ErrAppDataTooLarge  = fmt.Errorf("app data is at most %d bytes", MaxAppDataSize)
)

var appDataName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// appDataFile is how one piece of an app's data is kept: apps/<app>/<name>.json in the data folder, version 1.
type appDataFile struct {
	V       int             `json:"v"`
	Content json.RawMessage `json:"content"`
	SetAt   time.Time       `json:"setAt"`
	SetBy   string          `json:"setBy,omitempty"`
}

// AppData is a piece of data an app keeps on the server for the user, apart from any file: Audio Logs' labels,
// journal's tags. Each app sees only its own (the server files it under the device's app), so one app's labels never
// show in another. Version is the sha256 of the content, in hex, for SetAppData's base; empty when there is none.
type AppData struct {
	Content json.RawMessage
	Version string
	SetAt   time.Time
}

func (s *Store) appDataPath(app, name string) string {
	return filepath.Join(s.root, "apps", app, name+".json")
}

// AppData reads app's data called name; a zero AppData when there is none. A damaged file reads as none, and is
// replaced by the next SetAppData.
func (s *Store) AppData(app, name string) (AppData, error) {
	if !appDataName.MatchString(app) || !appDataName.MatchString(name) {
		return AppData{}, ErrAppDataName
	}
	data, err := os.ReadFile(s.appDataPath(app, name))
	if errors.Is(err, os.ErrNotExist) {
		return AppData{}, nil
	}
	if err != nil {
		return AppData{}, err
	}
	var file appDataFile
	if json.Unmarshal(data, &file) != nil || len(file.Content) == 0 {
		return AppData{}, nil
	}
	return AppData{Content: file.Content, Version: appDataVersion(file.Content), SetAt: file.SetAt}, nil
}

// SetAppData replaces app's data called name with content (a JSON object; empty deletes it), when base is the version
// the device changed (empty when it had none). When the data changed since, nothing is written and saved is false:
// now is the data kept, for the device to merge into and send again.
func (s *Store) SetAppData(app, name string, content []byte, base, setBy string) (now AppData, saved bool, err error) {
	if !appDataName.MatchString(app) || !appDataName.MatchString(name) {
		return AppData{}, false, ErrAppDataName
	}
	var compact bytes.Buffer
	if len(bytes.TrimSpace(content)) > 0 {
		var object map[string]json.RawMessage
		if json.Unmarshal(content, &object) != nil || object == nil {
			return AppData{}, false, ErrAppDataNotObject
		}
		if err := json.Compact(&compact, content); err != nil {
			return AppData{}, false, ErrAppDataNotObject
		}
		if compact.Len() > MaxAppDataSize {
			return AppData{}, false, ErrAppDataTooLarge
		}
	}
	err = s.Turn(func() error {
		current, err := s.AppData(app, name)
		if err != nil {
			return err
		}
		if current.Version != base {
			now = current
			return nil
		}
		path := s.appDataPath(app, name)
		if compact.Len() == 0 {
			saved = true
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			return nil
		}
		file := appDataFile{V: 1, Content: compact.Bytes(), SetAt: s.now().UTC(), SetBy: setBy}
		data, err := json.MarshalIndent(file, "", "  ")
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := writeFileAtomic(path, data); err != nil {
			return err
		}
		now, saved = AppData{Content: file.Content, Version: appDataVersion(file.Content), SetAt: file.SetAt}, true
		return nil
	})
	return now, saved, err
}

func appDataVersion(content json.RawMessage) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}
