package search

import (
	"bufio"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

//go:embed siglip_worker.py
var workerScript []byte

// PythonCommand is the worker the server runs: siglip_worker.py, written to the temporary folder, run by python with
// the model's name.
func PythonCommand(python, model string) ([]string, error) {
	script := filepath.Join(os.TempDir(), "journal-siglip-worker.py")
	if err := os.WriteFile(script, workerScript, 0o644); err != nil {
		return nil, err
	}
	return []string{python, script, model}, nil
}

// worker is the running model, one request at a time: a JSON line in, a JSON line out (see siglip_worker.py).
type worker struct {
	mu     sync.Mutex
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Scanner
	dims   int
	device string
	broken error // set once a request failed; the worker is then stopped and started again
}

type reply struct {
	Ready   bool      `json:"ready"`
	Dims    int       `json:"dims"`
	Device  string    `json:"device"`
	Vector  string    `json:"vector"`
	Vectors []*string `json:"vectors"`
	Errors  []*string `json:"errors"`
	Error   string    `json:"error"`
}

// startWorker runs command and waits for the model to load; the first start downloads it, which takes minutes.
func startWorker(command []string) (*worker, error) {
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", command[0], err)
	}
	w := &worker{cmd: cmd, in: in, out: bufio.NewScanner(out)}
	w.out.Buffer(nil, 16<<20)
	first, err := w.read()
	if err == nil && first.Error != "" {
		err = errors.New(first.Error)
	}
	if err == nil && (!first.Ready || first.Dims < 1) {
		err = fmt.Errorf("unexpected first answer %+v", first)
	}
	if err != nil {
		w.close()
		return nil, fmt.Errorf("the model didn't start (python3 -m pip install torch transformers pillow): %w", err)
	}
	w.dims, w.device = first.Dims, first.Device
	return w, nil
}

// images reads pictures; a picture that can't be read gets its own error and no vector.
func (w *worker) images(paths []string) (vectors [][]float32, errs []error, err error) {
	r, err := w.ask(map[string]any{"images": paths})
	if err != nil {
		return nil, nil, err
	}
	if len(r.Vectors) != len(paths) || len(r.Errors) != len(paths) {
		return nil, nil, w.fail(fmt.Errorf("%d answers for %d pictures", len(r.Vectors), len(paths)))
	}
	vectors, errs = make([][]float32, len(paths)), make([]error, len(paths))
	for i := range paths {
		switch {
		case r.Errors[i] != nil:
			errs[i] = errors.New(*r.Errors[i])
		case r.Vectors[i] == nil:
			errs[i] = errors.New("no vector")
		default:
			if vectors[i], err = w.decode(*r.Vectors[i]); err != nil {
				return nil, nil, err
			}
		}
	}
	return vectors, errs, nil
}

// text reads a query's words.
func (w *worker) text(words string) ([]float32, error) {
	r, err := w.ask(map[string]string{"text": words})
	if err != nil {
		return nil, err
	}
	return w.decode(r.Vector)
}

func (w *worker) ask(request any) (reply, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.broken != nil {
		return reply{}, w.broken
	}
	line, _ := json.Marshal(request)
	if _, err := w.in.Write(append(line, '\n')); err != nil {
		return reply{}, w.fail(err)
	}
	r, err := w.read()
	if err != nil {
		return reply{}, w.fail(err)
	}
	if r.Error != "" {
		// The request failed, not the worker (a picture batch the model choked on).
		return reply{}, errors.New(r.Error)
	}
	return r, nil
}

// fail marks the worker broken, for Engine.Run to start it again. Callers hold w.mu, or own w alone.
func (w *worker) fail(err error) error {
	w.broken = fmt.Errorf("the model stopped: %w", err)
	return w.broken
}

func (w *worker) isBroken() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.broken != nil
}

func (w *worker) read() (reply, error) {
	if !w.out.Scan() {
		if err := w.out.Err(); err != nil {
			return reply{}, err
		}
		return reply{}, io.ErrUnexpectedEOF
	}
	var r reply
	err := json.Unmarshal(w.out.Bytes(), &r)
	return r, err
}

func (w *worker) decode(vector string) ([]float32, error) {
	raw, err := base64.StdEncoding.DecodeString(vector)
	if err != nil || len(raw) != 4*w.dims {
		return nil, fmt.Errorf("a vector of %d bytes, not %d", len(raw), 4*w.dims)
	}
	v := make([]float32, w.dims)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[4*i:]))
	}
	return v, nil
}

func (w *worker) close() {
	w.in.Close()
	w.cmd.Process.Kill()
	w.cmd.Wait()
}
