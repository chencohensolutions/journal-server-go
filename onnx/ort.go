// Package onnx runs ONNX models on the CPU with ONNX Runtime: the search model (package search) and the face
// models (package faces).
//
// ONNX Runtime is a C library, loaded at start and called through its table of functions (OrtApi in
// onnxruntime_c_api.h) with purego, so the server still builds without cgo. Only the few functions needed to run
// a model on the CPU are used; their places in the table never change (new ones are only ever added at the end).
package onnx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"unsafe"

	"github.com/ebitengine/purego"
)

// The API version asked for: everything used here exists since ONNX Runtime 1.16.
const ortAPIVersion = 16

// Places in OrtApi, counted in onnxruntime_c_api.h (1.31.0).
const (
	ortGetErrorMessage                  = 2
	ortCreateEnv                        = 3
	ortCreateSession                    = 7
	ortRun                              = 9
	ortCreateSessionOptions             = 10
	ortSetSessionGraphOptimizationLevel = 23
	ortSetIntraOpNumThreads             = 24
	ortSetInterOpNumThreads             = 25
	ortSessionGetInputCount             = 30
	ortSessionGetOutputCount            = 31
	ortSessionGetInputName              = 36
	ortSessionGetOutputName             = 37
	ortCreateTensorWithDataAsOrtValue   = 49
	ortGetTensorMutableData             = 51
	ortGetTensorElementType             = 60
	ortGetDimensionsCount               = 61
	ortGetDimensions                    = 62
	ortGetTensorTypeAndShape            = 65
	ortCreateCpuMemoryInfo              = 69
	ortAllocatorFree                    = 76
	ortGetAllocatorWithDefaultOptions   = 78
	ortReleaseStatus                    = 93
	ortReleaseValue                     = 96
	ortReleaseTensorTypeAndShapeInfo    = 99
	ortReleaseSessionOptions            = 100
)

// Values of the C enums used.
const (
	ortLoggingLevelError    = 3  // OrtLoggingLevel
	ortEnableAll            = 99 // GraphOptimizationLevel
	ortDeviceAllocator      = 0  // OrtAllocatorType
	ortMemTypeDefault       = 0  // OrtMemType
	ortTensorFloat          = 1  // ONNXTensorElementDataType
	ortTensorInt64          = 7
	ortBaseGetVersionString = 1 // place of GetVersionString in OrtApiBase, after GetApi
)

// Runtime is ONNX Runtime, loaded once for the whole server.
type Runtime struct {
	api       unsafe.Pointer // OrtApi
	env       uintptr
	memory    uintptr // where input tensors live: plain CPU memory
	allocator uintptr // ONNX Runtime's default allocator, for the names it hands out
	Version   string
}

// FindRuntime is the ONNX Runtime library to load: configured (config.json "onnxruntime"), else the one
// scripts/bundle-onnx put in ./onnxruntime, else (on macOS and Linux) one installed in the usual places.
func FindRuntime(configured string) (string, error) {
	if configured != "" {
		if _, err := os.Stat(configured); err != nil {
			return "", fmt.Errorf("onnxruntime %s: %w", configured, err)
		}
		return filepath.Abs(configured)
	}
	for _, candidate := range append([]string{filepath.Join("onnxruntime", runtimeLibrary)}, installedRuntimes...) {
		if _, err := os.Stat(candidate); err == nil {
			return filepath.Abs(candidate)
		}
	}
	return "", errors.New("ONNX Runtime not found (go run ./scripts/bundle-onnx, or set \"onnxruntime\" in config.json)")
}

// RuntimeLibrary is the file name of the ONNX Runtime library on this system.
func RuntimeLibrary() string { return runtimeLibrary }

// LoadRuntime loads the ONNX Runtime library at path.
func LoadRuntime(path string) (*Runtime, error) {
	lib, err := loadLibrary(path)
	if err != nil {
		return nil, fmt.Errorf("loading %s: %w", path, err)
	}
	getAPIBase, err := findSymbol(lib, "OrtGetApiBase")
	if err != nil {
		return nil, fmt.Errorf("%s is not ONNX Runtime: %w", path, err)
	}
	base, _, _ := purego.SyscallN(getAPIBase)
	if base == 0 {
		return nil, fmt.Errorf("%s: OrtGetApiBase failed", path)
	}
	baseTable := toPointer(base)
	api, _, _ := purego.SyscallN(tableEntry(baseTable, 0), ortAPIVersion)
	version, _, _ := purego.SyscallN(tableEntry(baseTable, ortBaseGetVersionString))
	r := &Runtime{api: toPointer(api), Version: goString(version)}
	if api == 0 {
		return nil, fmt.Errorf("ONNX Runtime %s at %s is too old (1.16 or later is needed)", r.Version, path)
	}
	logID := cString("journal")
	if err := r.call(ortCreateEnv, ortLoggingLevelError, uintptr(unsafe.Pointer(&logID[0])), uintptr(unsafe.Pointer(&r.env))); err != nil {
		return nil, err
	}
	if err := r.call(ortCreateCpuMemoryInfo, ortDeviceAllocator, ortMemTypeDefault, uintptr(unsafe.Pointer(&r.memory))); err != nil {
		return nil, err
	}
	if err := r.call(ortGetAllocatorWithDefaultOptions, uintptr(unsafe.Pointer(&r.allocator))); err != nil {
		return nil, err
	}
	return r, nil
}

// call runs the function at place index in OrtApi, which answers with an OrtStatus: none when it worked.
//
//go:uintptrescapes
func (r *Runtime) call(index int, args ...uintptr) error {
	status, _, _ := purego.SyscallN(tableEntry(r.api, index), args...)
	if status == 0 {
		return nil
	}
	message, _, _ := purego.SyscallN(tableEntry(r.api, ortGetErrorMessage), status)
	err := errors.New("onnxruntime: " + goString(message))
	purego.SyscallN(tableEntry(r.api, ortReleaseStatus), status)
	return err
}

// release runs one of the Release functions, which answer nothing.
func (r *Runtime) release(index int, object uintptr) {
	if object != 0 {
		purego.SyscallN(tableEntry(r.api, index), object)
	}
}

// Session is one ONNX model, ready to run. Running it from several goroutines at once is safe.
type Session struct {
	rt              *Runtime
	handle          uintptr
	Inputs, Outputs []string // the names of the model's inputs and outputs
	input, output   string   // the ones Run gives and takes (Use)
}

// NewSession loads the model at path, run with up to threads CPU threads.
func (r *Runtime) NewSession(path string, threads int) (*Session, error) {
	var options uintptr
	if err := r.call(ortCreateSessionOptions, uintptr(unsafe.Pointer(&options))); err != nil {
		return nil, err
	}
	defer r.release(ortReleaseSessionOptions, options)
	if err := r.call(ortSetIntraOpNumThreads, options, uintptr(threads)); err != nil {
		return nil, err
	}
	if err := r.call(ortSetInterOpNumThreads, options, 1); err != nil {
		return nil, err
	}
	if err := r.call(ortSetSessionGraphOptimizationLevel, options, ortEnableAll); err != nil {
		return nil, err
	}
	modelPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	chars := pathChars(modelPath)
	s := &Session{rt: r}
	err = r.call(ortCreateSession, r.env, uintptr(chars), options, uintptr(unsafe.Pointer(&s.handle)))
	runtime.KeepAlive(chars)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if s.Inputs, err = s.names(ortSessionGetInputCount, ortSessionGetInputName); err != nil {
		return nil, err
	}
	if s.Outputs, err = s.names(ortSessionGetOutputCount, ortSessionGetOutputName); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Session) names(count, name int) ([]string, error) {
	var n uintptr
	if err := s.rt.call(count, s.handle, uintptr(unsafe.Pointer(&n))); err != nil {
		return nil, err
	}
	names := make([]string, n)
	for i := range names {
		var p uintptr
		if err := s.rt.call(name, s.handle, uintptr(i), s.rt.allocator, uintptr(unsafe.Pointer(&p))); err != nil {
			return nil, err
		}
		names[i] = goString(p)
		s.rt.call(ortAllocatorFree, s.rt.allocator, p)
	}
	return names, nil
}

// Use picks the input and output that Run gives and takes: the first of the wanted names the model has.
func (s *Session) Use(inputs, outputs []string) error {
	pick := func(have, want []string) string {
		for _, w := range want {
			if slices.Contains(have, w) {
				return w
			}
		}
		return ""
	}
	s.input, s.output = pick(s.Inputs, inputs), pick(s.Outputs, outputs)
	if s.input == "" || s.output == "" {
		return fmt.Errorf("the model takes %v and gives %v; expected one of %v and one of %v", s.Inputs, s.Outputs, inputs, outputs)
	}
	return nil
}

// Tensor is the data of one input or output and its shape.
type Tensor[T float32 | int64] struct {
	Data  []T
	Shape []int64
}

// Run gives the model the input Use picked and returns the output it picked, which must be floats.
func Run[T float32 | int64](s *Session, in Tensor[T]) (Tensor[float32], error) {
	out, err := RunNamed(s, s.input, in, s.output)
	if err != nil {
		return Tensor[float32]{}, err
	}
	return out[0], nil
}

// RunNamed gives the model in as its input named input and returns its outputs named outputs, in that order,
// which must be floats.
func RunNamed[T float32 | int64](s *Session, input string, in Tensor[T], outputs ...string) ([]Tensor[float32], error) {
	r := s.rt
	elementType := uintptr(ortTensorFloat)
	if _, ok := any(in.Data).([]int64); ok {
		elementType = ortTensorInt64
	}
	var value uintptr
	err := r.call(ortCreateTensorWithDataAsOrtValue, r.memory,
		uintptr(unsafe.Pointer(&in.Data[0])), uintptr(len(in.Data))*unsafe.Sizeof(in.Data[0]),
		uintptr(unsafe.Pointer(&in.Shape[0])), uintptr(len(in.Shape)), elementType, uintptr(unsafe.Pointer(&value)))
	if err != nil {
		return nil, err
	}
	defer r.release(ortReleaseValue, value)
	inputName := cString(input)
	inputNames := []uintptr{uintptr(unsafe.Pointer(&inputName[0]))}
	names := make([][]byte, len(outputs))
	outputNames := make([]uintptr, len(outputs))
	for i, o := range outputs {
		names[i] = cString(o)
		outputNames[i] = uintptr(unsafe.Pointer(&names[i][0]))
	}
	values := make([]uintptr, len(outputs))
	err = r.call(ortRun, s.handle, 0, uintptr(unsafe.Pointer(&inputNames[0])), uintptr(unsafe.Pointer(&value)), 1,
		uintptr(unsafe.Pointer(&outputNames[0])), uintptr(len(outputs)), uintptr(unsafe.Pointer(&values[0])))
	// The names are held as plain numbers above, which the garbage collector doesn't see.
	runtime.KeepAlive(inputName)
	runtime.KeepAlive(names)
	runtime.KeepAlive(in.Data)
	for _, v := range values {
		defer r.release(ortReleaseValue, v)
	}
	if err != nil {
		return nil, err
	}
	out := make([]Tensor[float32], len(values))
	for i, v := range values {
		if out[i], err = r.floats(v); err != nil {
			return nil, fmt.Errorf("output %s: %w", outputs[i], err)
		}
	}
	return out, nil
}

// floats copies out the tensor value, which must hold floats.
func (r *Runtime) floats(value uintptr) (Tensor[float32], error) {
	var info, count uintptr
	if err := r.call(ortGetTensorTypeAndShape, value, uintptr(unsafe.Pointer(&info))); err != nil {
		return Tensor[float32]{}, err
	}
	defer r.release(ortReleaseTensorTypeAndShapeInfo, info)
	var elementType int32
	if err := r.call(ortGetTensorElementType, info, uintptr(unsafe.Pointer(&elementType))); err != nil {
		return Tensor[float32]{}, err
	}
	if elementType != ortTensorFloat {
		return Tensor[float32]{}, fmt.Errorf("holds elements of type %d, not floats", elementType)
	}
	if err := r.call(ortGetDimensionsCount, info, uintptr(unsafe.Pointer(&count))); err != nil {
		return Tensor[float32]{}, err
	}
	out := Tensor[float32]{Shape: make([]int64, count)}
	if count > 0 {
		if err := r.call(ortGetDimensions, info, uintptr(unsafe.Pointer(&out.Shape[0])), count); err != nil {
			return Tensor[float32]{}, err
		}
	}
	size := int64(1)
	for _, d := range out.Shape {
		size *= d
	}
	var data uintptr
	if err := r.call(ortGetTensorMutableData, value, uintptr(unsafe.Pointer(&data))); err != nil {
		return Tensor[float32]{}, err
	}
	out.Data = make([]float32, size)
	if size > 0 {
		copy(out.Data, unsafe.Slice((*float32)(toPointer(data)), size))
	}
	return out, nil
}

// tableEntry is the function pointer at place index of a C table of them.
func tableEntry(table unsafe.Pointer, index int) uintptr {
	return *(*uintptr)(unsafe.Add(table, index*int(unsafe.Sizeof(uintptr(0)))))
}

// toPointer turns an address ONNX Runtime returned (C memory, never moved) into a pointer.
func toPointer(address uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&address))
}

func cString(s string) []byte { return append([]byte(s), 0) }

// goString copies the C string at address.
func goString(address uintptr) string {
	if address == 0 {
		return ""
	}
	p := toPointer(address)
	n := 0
	for *(*byte)(unsafe.Add(p, n)) != 0 {
		n++
	}
	return string(unsafe.Slice((*byte)(p), n))
}
