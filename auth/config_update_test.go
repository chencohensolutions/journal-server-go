package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadConfigText(t *testing.T, text string) (AppConfig, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return LoadConfig(path)
}

func TestUpdatesAreAutomaticAtFourByDefault(t *testing.T) {
	config, err := loadConfigText(t, `{"users": [{"username": "hen", "data_folder": "data"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if config.Update != UpdateAuto || config.UpdateAt != "04:00" {
		t.Errorf("update = %q at %q, want %q at 04:00", config.Update, config.UpdateAt, UpdateAuto)
	}
}

func TestAnUnknownUpdateSettingIsRefused(t *testing.T) {
	for _, setting := range []string{`"update": "sometimes"`, `"update_at": "4am"`} {
		_, err := loadConfigText(t, `{"users": [{"username": "hen", "data_folder": "data"}], `+setting+`}`)
		if err == nil || !strings.Contains(err.Error(), "update") {
			t.Errorf("%s: LoadConfig = %v, want an error naming the setting", setting, err)
		}
	}
}
