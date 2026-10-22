//go:build darwin || linux || freebsd || netbsd

package onnx

import (
	"runtime"
	"unsafe"

	"github.com/ebitengine/purego"
)

var runtimeLibrary = func() string {
	if runtime.GOOS == "darwin" {
		return "libonnxruntime.dylib"
	}
	return "libonnxruntime.so"
}()

// Where Homebrew (brew install onnxruntime) and Linux packages put it.
var installedRuntimes = []string{
	"/opt/homebrew/lib/" + runtimeLibrary,
	"/usr/local/lib/" + runtimeLibrary,
	"/usr/lib/" + runtimeLibrary,
}

func loadLibrary(path string) (uintptr, error) {
	return purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
}

func findSymbol(lib uintptr, name string) (uintptr, error) {
	return purego.Dlsym(lib, name)
}

// pathChars is path as ONNX Runtime takes file names outside Windows: UTF-8, ending in a zero.
func pathChars(path string) unsafe.Pointer {
	return unsafe.Pointer(&cString(path)[0])
}
