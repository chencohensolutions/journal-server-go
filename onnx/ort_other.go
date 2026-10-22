//go:build !darwin && !linux && !freebsd && !netbsd && !windows

package onnx

import (
	"errors"
	"unsafe"
)

const runtimeLibrary = "libonnxruntime.so"

var installedRuntimes []string

func loadLibrary(string) (uintptr, error) {
	return 0, errors.New("loading ONNX Runtime is not supported on this system")
}

func findSymbol(uintptr, string) (uintptr, error) { return 0, errors.ErrUnsupported }

func pathChars(path string) unsafe.Pointer { return unsafe.Pointer(&cString(path)[0]) }
