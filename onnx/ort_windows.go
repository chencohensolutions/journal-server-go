package onnx

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

const runtimeLibrary = "onnxruntime.dll"

// None: Windows itself ships an old onnxruntime.dll (for Windows ML) that must not be picked up.
var installedRuntimes []string

func loadLibrary(path string) (uintptr, error) {
	// The altered search path finds onnxruntime_providers_shared.dll beside it.
	h, err := windows.LoadLibraryEx(path, 0, windows.LOAD_WITH_ALTERED_SEARCH_PATH)
	return uintptr(h), err
}

func findSymbol(lib uintptr, name string) (uintptr, error) {
	return windows.GetProcAddress(windows.Handle(lib), name)
}

// pathChars is path as ONNX Runtime takes file names on Windows: UTF-16, ending in a zero.
func pathChars(path string) unsafe.Pointer {
	chars, err := windows.UTF16FromString(path)
	if err != nil { // a zero inside: the name can't exist
		chars = []uint16{0}
	}
	return unsafe.Pointer(&chars[0])
}
