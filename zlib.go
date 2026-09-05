package pyodide

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"io"
	"strconv"
	"sync"

	experimentalsys "github.com/tetratelabs/wazero/experimental/sys"
)

// The zlib device implements Python's zlib module on top of Go's compress
// packages, because the CPython WASI build ships without zlib. The Python
// side is patches/zlib.py. Paths, relative to /dev/pyodide:
//
//	zlib/deflate/<level>/<wbits>[/dict]   compressobj
//	zlib/inflate/<wbits>[/dict]           decompressobj
//	zlib/adler32/<start>                  adler32
//
// Writes feed input, reads drain output. Datasync flushes (Z_SYNC_FLUSH) and
// Sync finishes (Z_FINISH) a stream. With a trailing /dict, everything written
// before the first Datasync is the preset dictionary.
//
// Status (pread) is one flag byte followed by a payload: bit 0 = end of
// stream reached (payload: unused input after the stream), bit 1 = output
// pending, bit 2 = error (payload: message).
const (
	statusEOF     = 1 << 0
	statusPending = 1 << 1
	statusError   = 1 << 2
)

func openZlibDevice(args []string) (device, experimentalsys.Errno) {
	if len(args) == 0 {
		return nil, experimentalsys.ENOENT
	}
	ints := func(n int) ([]int, bool) {
		rest := args[1:]
		dict := len(rest) == n+1 && rest[n] == "dict"
		if len(rest) != n && !dict {
			return nil, false
		}
		out := make([]int, n)
		for i := range n {
			v, err := strconv.Atoi(rest[i])
			if err != nil {
				return nil, false
			}
			out[i] = v
		}
		return out, dict
	}
	switch args[0] {
	case "deflate":
		v, dict := ints(2)
		if v == nil {
			return nil, experimentalsys.ENOENT
		}
		return &deflater{level: v[0], wbits: v[1], dictMode: dict}, 0
	case "inflate":
		v, dict := ints(1)
		if v == nil {
			return nil, experimentalsys.ENOENT
		}
		return newInflater(v[0], dict), 0
	case "adler32":
		v, _ := ints(1)
		if v == nil {
			return nil, experimentalsys.ENOENT
		}
		return &adler{a: uint32(v[0]) & 0xffff, b: uint32(v[0]) >> 16 & 0xffff}, 0
	}
	return nil, experimentalsys.ENOENT
}

// wbits semantics shared with zlib: |wbits| in 8..15 selects the format by
// sign (raw deflate when negative), +16 selects gzip, +32 auto-detects zlib
// or gzip when reading. 0 means zlib with the window size from the header.
func format(wbits int) (raw, gz, auto bool) {
	switch {
	case wbits < 0:
		return true, false, false
	case wbits >= 32:
		return false, false, true
	case wbits >= 16:
		return false, true, false
	}
	return false, false, false
}

// deflater is a compressobj.
type deflater struct {
	level    int
	wbits    int
	dictMode bool
	dict     []byte

	w   flusher
	out bytes.Buffer
	err error
	fin bool
}

type flusher interface {
	io.WriteCloser
	Flush() error
}

func (d *deflater) start() error {
	raw, gz, _ := format(d.wbits)
	var err error
	switch {
	case raw:
		d.w, err = flate.NewWriterDict(&d.out, d.level, d.dict)
	case gz:
		d.w, err = gzip.NewWriterLevel(&d.out, d.level)
	default:
		d.w, err = zlib.NewWriterLevelDict(&d.out, d.level, d.dict)
	}
	return err
}

func (d *deflater) Write(p []byte) (int, experimentalsys.Errno) {
	if d.err != nil || d.fin {
		return 0, experimentalsys.EIO
	}
	if d.dictMode {
		d.dict = append(d.dict, p...)
		return len(p), 0
	}
	if d.w == nil {
		if d.err = d.start(); d.err != nil {
			return 0, experimentalsys.EIO
		}
	}
	n, err := d.w.Write(p)
	if err != nil {
		d.err = err
		return n, experimentalsys.EIO
	}
	return n, 0
}

func (d *deflater) Read(p []byte) (int, experimentalsys.Errno) {
	if d.err != nil {
		return 0, experimentalsys.EIO
	}
	n, _ := d.out.Read(p)
	return n, 0
}

func (d *deflater) Datasync() experimentalsys.Errno {
	if d.dictMode {
		d.dictMode = false
		return 0
	}
	if d.err != nil || d.fin {
		return experimentalsys.EIO
	}
	if d.w == nil {
		if d.err = d.start(); d.err != nil {
			return experimentalsys.EIO
		}
	}
	if d.err = d.w.Flush(); d.err != nil {
		return experimentalsys.EIO
	}
	return 0
}

func (d *deflater) Sync() experimentalsys.Errno {
	if d.err != nil {
		return experimentalsys.EIO
	}
	if d.fin {
		return 0
	}
	d.dictMode = false
	if d.w == nil {
		if d.err = d.start(); d.err != nil {
			return experimentalsys.EIO
		}
	}
	d.fin = true
	if d.err = d.w.Close(); d.err != nil {
		return experimentalsys.EIO
	}
	return 0
}

func (d *deflater) Truncate(int64) experimentalsys.Errno { return 0 }

func (d *deflater) Status() []byte {
	return status(d.fin, d.out.Len() > 0, d.err, nil)
}

func (d *deflater) Close() experimentalsys.Errno { return 0 }

func status(eof, pending bool, err error, unused []byte) []byte {
	var flags byte
	if eof {
		flags |= statusEOF
	}
	if pending {
		flags |= statusPending
	}
	if err != nil {
		flags |= statusError
		return append([]byte{flags}, err.Error()...)
	}
	return append([]byte{flags}, unused...)
}

// inflater is a decompressobj. Go's decompressors pull from a reader, so the
// decoder runs in its own goroutine and is driven synchronously: Write hands
// it input and returns once it is blocked again, either waiting for input,
// finished, or having filled its output budget. The budget (Python's
// max_length, set through Truncate) is what makes unconsumed input exact: when
// the decoder stops early, whatever it has not pulled is handed back through
// Status and becomes Python's unconsumed_tail.
type inflater struct {
	wbits    int
	dictMode bool
	dict     []byte

	mu   sync.Mutex
	cond *sync.Cond

	in       []byte // input not yet pulled by the decoder
	closed   bool   // no more input will arrive
	idle     bool   // decoder is blocked waiting for input
	full     bool   // decoder is blocked on the output budget
	limit    int    // output budget in bytes, 0 = unlimited
	started  bool
	finished bool

	out bytes.Buffer
	eof bool
	err error
}

func newInflater(wbits int, dict bool) *inflater {
	i := &inflater{wbits: wbits, dictMode: dict}
	i.cond = sync.NewCond(&i.mu)
	return i
}

// source is the decoder goroutine's view of the input. Implementing
// io.ByteReader keeps the decoders from buffering ahead, so leftover input
// after the end of a stream is known exactly (Python's unused_data).
type source struct {
	head []byte // bytes read for format detection, replayed first
	i    *inflater
}

func (s *source) Read(p []byte) (int, error) {
	if len(s.head) > 0 {
		n := copy(p, s.head)
		s.head = s.head[n:]
		return n, nil
	}
	i := s.i
	i.mu.Lock()
	defer i.mu.Unlock()
	for len(i.in) == 0 && !i.closed {
		i.idle = true
		i.cond.Broadcast()
		i.cond.Wait()
	}
	i.idle = false
	if len(i.in) == 0 {
		return 0, io.EOF
	}
	n := copy(p, i.in)
	i.in = i.in[n:]
	return n, nil
}

func (s *source) ReadByte() (byte, error) {
	var b [1]byte
	if _, err := s.Read(b[:]); err != nil {
		return 0, err
	}
	return b[0], nil
}

func (i *inflater) open(src *source) (io.Reader, error) {
	raw, gz, auto := format(i.wbits)
	if auto {
		var head [2]byte
		if _, err := io.ReadFull(src, head[:]); err != nil {
			return nil, err
		}
		src.head = head[:]
		gz = head[0] == 0x1f && head[1] == 0x8b
	}
	switch {
	case raw:
		return flate.NewReaderDict(src, i.dict), nil
	case gz:
		r, err := gzip.NewReader(src)
		if err != nil {
			return nil, err
		}
		r.Multistream(false)
		return r, nil
	}
	return zlib.NewReaderDict(src, i.dict)
}

func (i *inflater) run() {
	r, err := i.open(&source{i: i})
	if err == nil {
		buf := make([]byte, 64<<10)
		for {
			// Respect the output budget before pulling more input.
			i.mu.Lock()
			for i.limit > 0 && i.out.Len() >= i.limit && !i.closed {
				i.full = true
				i.cond.Broadcast()
				i.cond.Wait()
			}
			i.full = false
			n := len(buf)
			if i.limit > 0 {
				n = min(n, i.limit-i.out.Len())
			}
			i.mu.Unlock()

			n, err = r.Read(buf[:n])
			i.mu.Lock()
			i.out.Write(buf[:n])
			i.mu.Unlock()
			if err != nil {
				break
			}
		}
	}
	i.mu.Lock()
	if errors.Is(err, io.EOF) {
		i.eof = true
	} else {
		i.err = err
	}
	i.finished = true
	i.cond.Broadcast()
	i.mu.Unlock()
}

// resume wakes the decoder and waits until it blocks again. Callers hold mu.
func (i *inflater) resume() {
	if !i.started {
		i.started = true
		go i.run()
	}
	i.idle, i.full = false, false
	i.cond.Broadcast()
	for !i.idle && !i.full && !i.finished {
		i.cond.Wait()
	}
}

func (i *inflater) Write(p []byte) (int, experimentalsys.Errno) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.dictMode {
		i.dict = append(i.dict, p...)
		return len(p), 0
	}
	if i.err != nil || i.closed {
		return 0, experimentalsys.EIO
	}
	i.in = append(i.in, p...)
	if i.finished {
		// Past the end of the stream: the bytes stay as unused data.
		return len(p), 0
	}
	i.resume()
	if i.err != nil {
		return len(p), experimentalsys.EIO
	}
	return len(p), 0
}

func (i *inflater) Read(p []byte) (int, experimentalsys.Errno) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.err != nil {
		return 0, experimentalsys.EIO
	}
	n, _ := i.out.Read(p)
	return n, 0
}

// Truncate sets the output budget (0 = unlimited) and lets the decoder
// continue if it had stopped on the previous budget.
func (i *inflater) Truncate(size int64) experimentalsys.Errno {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.err != nil {
		return experimentalsys.EIO
	}
	i.limit = int(size)
	if i.started && !i.finished && !i.closed {
		i.resume()
	}
	if i.err != nil {
		return experimentalsys.EIO
	}
	return 0
}

// Datasync ends dictionary mode; otherwise it is a no-op for decompression.
func (i *inflater) Datasync() experimentalsys.Errno {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.dictMode = false
	return 0
}

// Sync declares the input complete: the stream must have ended.
func (i *inflater) Sync() experimentalsys.Errno {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.dictMode = false
	if i.err != nil {
		return experimentalsys.EIO
	}
	i.closed = true
	i.limit = 0
	if !i.started {
		i.started = true
		go i.run()
	}
	i.cond.Broadcast()
	for !i.finished {
		i.cond.Wait()
	}
	if i.err == nil && !i.eof {
		i.err = errors.New("incomplete or truncated stream")
	}
	if i.err != nil {
		return experimentalsys.EIO
	}
	return 0
}

// Status hands the input the decoder has not pulled back to the caller: it
// is unused_data after the end of the stream, unconsumed_tail before.
func (i *inflater) Status() []byte {
	i.mu.Lock()
	defer i.mu.Unlock()
	rec := status(i.eof, i.out.Len() > 0, i.err, i.in)
	if !i.eof && i.err == nil {
		i.in = nil
	}
	return rec
}

func (i *inflater) Close() experimentalsys.Errno {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.closed = true
	i.limit = 0
	i.cond.Broadcast()
	for i.started && !i.finished {
		i.cond.Wait()
	}
	return 0
}

// adler computes adler32 with an arbitrary start value.
type adler struct {
	a, b uint32
}

func (c *adler) Write(p []byte) (int, experimentalsys.Errno) {
	const mod = 65521
	a, b := c.a, c.b
	total := len(p)
	for len(p) > 0 {
		// 5552 is the largest n such that the sums cannot overflow uint32.
		n := min(len(p), 5552)
		for _, x := range p[:n] {
			a += uint32(x)
			b += a
		}
		a %= mod
		b %= mod
		p = p[n:]
	}
	c.a, c.b = a, b
	return total, 0
}

func (c *adler) Read(p []byte) (int, experimentalsys.Errno) { return 0, 0 }

func (c *adler) Status() []byte {
	v := c.b<<16 | c.a
	return []byte{0, byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}
}

func (c *adler) Truncate(int64) experimentalsys.Errno { return 0 }
func (c *adler) Datasync() experimentalsys.Errno      { return 0 }
func (c *adler) Sync() experimentalsys.Errno          { return 0 }
func (c *adler) Close() experimentalsys.Errno         { return 0 }
