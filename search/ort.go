package search

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"unsafe"

	"github.com/ebitengine/purego"
)

// ONNX Runtime is a C library, loaded at start and called through its table of functions (OrtApi in
// onnxruntime_c_api.h) with purego, so the server still builds without cgo. Only the few functions needed to run
// a model on the CPU are used; their places in the table never change (new ones are only ever added at the end).

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

// session is one ONNX model, ready to run. Running it from several goroutines at once is safe.
type session struct {
	rt                    *Runtime
	handle                uintptr
	inputs, outputs       []string
	inputName, outputName []byte // the ones run, as C strings
}

// newSession loads the model at path, run with up to threads CPU threads.
func (r *Runtime) newSession(path string, threads int) (*session, error) {
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
	s := &session{rt: r}
	err = r.call(ortCreateSession, r.env, uintptr(chars), options, uintptr(unsafe.Pointer(&s.handle)))
	runtime.KeepAlive(chars)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if s.inputs, err = s.names(ortSessionGetInputCount, ortSessionGetInputName); err != nil {
		return nil, err
	}
	if s.outputs, err = s.names(ortSessionGetOutputCount, ortSessionGetOutputName); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *session) names(count, name int) ([]string, error) {
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

// use picks the input and output that run gives and takes: the first of the wanted names the model has.
func (s *session) use(inputs, outputs []string) error {
	pick := func(have, want []string) []byte {
		for _, w := range want {
			for _, h := range have {
				if h == w {
					return cString(h)
				}
			}
		}
		return nil
	}
	s.inputName, s.outputName = pick(s.inputs, inputs), pick(s.outputs, outputs)
	if s.inputName == nil || s.outputName == nil {
		return fmt.Errorf("the model takes %v and gives %v; expected one of %v and one of %v", s.inputs, s.outputs, inputs, outputs)
	}
	return nil
}

// tensor is the data of one input or output and its shape.
type tensor[T float32 | int64] struct {
	data  []T
	shape []int64
}

// run gives the model one input and returns its output, which must be floats.
func run[T float32 | int64](s *session, in tensor[T]) (tensor[float32], error) {
	r := s.rt
	elementType := uintptr(ortTensorFloat)
	if _, ok := any(in.data).([]int64); ok {
		elementType = ortTensorInt64
	}
	var input, output uintptr
	err := r.call(ortCreateTensorWithDataAsOrtValue, r.memory,
		uintptr(unsafe.Pointer(&in.data[0])), uintptr(len(in.data))*unsafe.Sizeof(in.data[0]),
		uintptr(unsafe.Pointer(&in.shape[0])), uintptr(len(in.shape)), elementType, uintptr(unsafe.Pointer(&input)))
	if err != nil {
		return tensor[float32]{}, err
	}
	defer r.release(ortReleaseValue, input)
	inputNames := []uintptr{uintptr(unsafe.Pointer(&s.inputName[0]))}
	outputNames := []uintptr{uintptr(unsafe.Pointer(&s.outputName[0]))}
	err = r.call(ortRun, s.handle, 0, uintptr(unsafe.Pointer(&inputNames[0])), uintptr(unsafe.Pointer(&input)), 1,
		uintptr(unsafe.Pointer(&outputNames[0])), 1, uintptr(unsafe.Pointer(&output)))
	// The names are held as plain numbers above, which the garbage collector doesn't see.
	runtime.KeepAlive(s)
	runtime.KeepAlive(in.data)
	if err != nil {
		return tensor[float32]{}, err
	}
	defer r.release(ortReleaseValue, output)

	var info, count uintptr
	if err := r.call(ortGetTensorTypeAndShape, output, uintptr(unsafe.Pointer(&info))); err != nil {
		return tensor[float32]{}, err
	}
	defer r.release(ortReleaseTensorTypeAndShapeInfo, info)
	if err := r.call(ortGetDimensionsCount, info, uintptr(unsafe.Pointer(&count))); err != nil {
		return tensor[float32]{}, err
	}
	out := tensor[float32]{shape: make([]int64, count)}
	if count > 0 {
		if err := r.call(ortGetDimensions, info, uintptr(unsafe.Pointer(&out.shape[0])), count); err != nil {
			return tensor[float32]{}, err
		}
	}
	size := int64(1)
	for _, d := range out.shape {
		size *= d
	}
	var data uintptr
	if err := r.call(ortGetTensorMutableData, output, uintptr(unsafe.Pointer(&data))); err != nil {
		return tensor[float32]{}, err
	}
	out.data = make([]float32, size)
	copy(out.data, unsafe.Slice((*float32)(toPointer(data)), size))
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
