package tensor

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
)

// Weights are the tensors of a .safetensors file, read on demand (the file stays open until Close).
type Weights struct {
	f       *os.File
	base    int64
	entries map[string]entry
	used    map[string]bool
}

type entry struct {
	DType   string   `json:"dtype"`
	Shape   []int    `json:"shape"`
	Offsets [2]int64 `json:"data_offsets"`
}

// OpenSafetensors reads the header of a .safetensors file.
func OpenSafetensors(path string) (*Weights, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	var n uint64
	if err := binary.Read(f, binary.LittleEndian, &n); err != nil || n > 100<<20 {
		f.Close()
		return nil, fmt.Errorf("%s: not a safetensors file", path)
	}
	head := make([]byte, n)
	if _, err := io.ReadFull(f, head); err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(head, &raw); err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: header: %w", path, err)
	}
	w := &Weights{f: f, base: 8 + int64(n), entries: map[string]entry{}, used: map[string]bool{}}
	for k, v := range raw {
		if k == "__metadata__" {
			continue
		}
		var e entry
		if err := json.Unmarshal(v, &e); err != nil {
			f.Close()
			return nil, fmt.Errorf("%s: tensor %s: %w", path, k, err)
		}
		w.entries[k] = e
	}
	return w, nil
}

// Close closes the file.
func (w *Weights) Close() error { return w.f.Close() }

// Has reports whether the file has the tensor.
func (w *Weights) Has(name string) bool { _, ok := w.entries[name]; return ok }

// Shape is the tensor's shape (nil if missing).
func (w *Weights) Shape(name string) []int { return w.entries[name].Shape }

// Unused lists the tensors nobody asked for (a check that a model reads what it should).
func (w *Weights) Unused() []string {
	var out []string
	for k := range w.entries {
		if !w.used[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func (w *Weights) raw(name string, shape ...int) (entry, []byte, error) {
	e, ok := w.entries[name]
	if !ok {
		return e, nil, fmt.Errorf("tensor %s missing", name)
	}
	if len(shape) > 0 {
		if len(shape) != len(e.Shape) {
			return e, nil, fmt.Errorf("tensor %s: shape %v, want %v", name, e.Shape, shape)
		}
		for i := range shape {
			if shape[i] != e.Shape[i] {
				return e, nil, fmt.Errorf("tensor %s: shape %v, want %v", name, e.Shape, shape)
			}
		}
	}
	buf := make([]byte, e.Offsets[1]-e.Offsets[0])
	if _, err := w.f.ReadAt(buf, w.base+e.Offsets[0]); err != nil {
		return e, nil, fmt.Errorf("tensor %s: %w", name, err)
	}
	w.used[name] = true
	return e, buf, nil
}

// F32 reads a tensor as float32 (F32, F16 or BF16 in the file), checking its shape when given. With a GPU the values
// go straight into its memory (newWeights), where the CPU reads them too.
func (w *Weights) F32(name string, shape ...int) ([]float32, error) {
	e, buf, err := w.raw(name, shape...)
	if err != nil {
		return nil, err
	}
	switch e.DType {
	case "F32":
		out := newWeights(len(buf) / 4)
		for i := range out {
			out[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4:]))
		}
		return out, nil
	case "F16", "BF16":
		h := Half{BF16: e.DType == "BF16", Data: buf}
		out := newWeights(h.Len())
		for i := range out {
			out[i] = h.At(i)
		}
		return out, nil
	}
	return nil, fmt.Errorf("tensor %s: dtype %s not supported", name, e.DType)
}

// Half reads a 16-bit tensor as it is (kept small: the token embeddings, looked up a row at a time).
func (w *Weights) Half(name string, shape ...int) (Half, error) {
	e, buf, err := w.raw(name, shape...)
	if err != nil {
		return Half{}, err
	}
	switch e.DType {
	case "F16", "BF16":
		return Half{BF16: e.DType == "BF16", Data: buf}, nil
	case "F32":
		out := Half{F32: make([]float32, len(buf)/4)}
		for i := range out.F32 {
			out.F32[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4:]))
		}
		return out, nil
	}
	return Half{}, fmt.Errorf("tensor %s: dtype %s not supported", name, e.DType)
}

// Half is a 16-bit float tensor (IEEE half or bfloat16), little endian — or, from a float32 file, the values.
type Half struct {
	BF16 bool
	Data []byte
	F32  []float32
}

// Len is the number of values.
func (h Half) Len() int {
	if h.F32 != nil {
		return len(h.F32)
	}
	return len(h.Data) / 2
}

// At is the i-th value as float32.
func (h Half) At(i int) float32 {
	if h.F32 != nil {
		return h.F32[i]
	}
	b := binary.LittleEndian.Uint16(h.Data[i*2:])
	if h.BF16 {
		return math.Float32frombits(uint32(b) << 16)
	}
	return f16ToF32(b)
}

// Row copies values [i·d, (i+1)·d) into dst.
func (h Half) Row(i, d int, dst []float32) {
	for j := 0; j < d; j++ {
		dst[j] = h.At(i*d + j)
	}
}

func f16ToF32(h uint16) float32 {
	sign := uint32(h>>15) << 31
	exp := uint32(h>>10) & 0x1f
	man := uint32(h) & 0x3ff
	switch {
	case exp == 0:
		if man == 0 {
			return math.Float32frombits(sign)
		}
		// subnormal: normalise
		e := uint32(127 - 15 + 1)
		for man&0x400 == 0 {
			man <<= 1
			e--
		}
		man &= 0x3ff
		return math.Float32frombits(sign | e<<23 | man<<13)
	case exp == 0x1f:
		return math.Float32frombits(sign | 0xff<<23 | man<<13)
	}
	return math.Float32frombits(sign | (exp+127-15)<<23 | man<<13)
}
