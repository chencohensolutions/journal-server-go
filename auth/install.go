package auth

import (
	"os"
	"path/filepath"
)

// UseInstallFolder lets the installed program (journal-server, with its import and jxl) run from any folder:
// when configPath is relative and isn't in the current folder but is beside the running program, it changes to
// the program's folder, where config.json's relative paths (certs/, models/, vips/, onnxruntime/) also point. It
// changes nothing when the current folder has the file, so running from the source folder works as before.
// Programs that take folders as arguments make them absolute first. It returns the folder it changed to, or "".
func UseInstallFolder(configPath string) (string, error) {
	if filepath.IsAbs(configPath) {
		return "", nil
	}
	if _, err := os.Stat(configPath); err == nil {
		return "", nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	dir := filepath.Dir(exe)
	if _, err := os.Stat(filepath.Join(dir, configPath)); err != nil {
		return "", nil
	}
	return dir, os.Chdir(dir)
}
