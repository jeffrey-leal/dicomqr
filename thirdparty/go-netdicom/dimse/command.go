package dimse

import (
	"io"
	"os"
	"sync"
)

// DimseCommand holds one DIMSE message's data set as it arrives, fragment by
// fragment, and hands it to the handler that consumes it.
//
// dicomqr local patch: upstream spooled every data set to a temp file,
// reopening that file for every P-DATA fragment appended — so every C-FIND
// response and every received image went through %TEMP%, a CreateFile and
// CloseHandle per PDU, and the application then copied each image a second
// time into its download folder. Now a data set is held in memory up to
// memoryLimit — every C-FIND identifier, and single-frame CT, MR, NM and US
// images — and only a larger one (radiographs, multi-frame acquisitions)
// spills to a temp file, which is then kept open across fragments rather than
// reopened. The limit keeps what several concurrent associations can hold in
// memory bounded.
//
// NewDimseCommand keeps the original file-backed form for callers that name
// their own file; NewBufferedDimseCommand is the memory-first form the
// command assembler uses. The methods behave the same in both.
type DimseCommand struct {
	mu sync.Mutex

	mem    []byte // the data set, while buffered in memory (fpath == "")
	memPos int    // read position in mem

	fpath string   // backing file, once spilled or when file-backed from the start
	w     *os.File // append handle, kept open across fragments
	r     *os.File // read handle
}

// memoryLimit is the largest data set held in memory; a var so tests can
// exercise the spill.
var memoryLimit = 8 << 20

// NewDimseCommand returns a file-backed command whose data lives at fpath.
func NewDimseCommand(fpath string) *DimseCommand {
	return &DimseCommand{fpath: fpath}
}

// NewBufferedDimseCommand returns a command that holds its data set in memory,
// spilling to a temp file only past memoryLimit.
func NewBufferedDimseCommand() *DimseCommand {
	return &DimseCommand{}
}

// AppendData adds one fragment of the data set.
func (dc *DimseCommand) AppendData(data []byte) error {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	if dc.fpath == "" {
		if len(dc.mem)+len(data) <= memoryLimit {
			dc.mem = append(dc.mem, data...)
			return nil
		}
		// Past the limit: move what has arrived so far to a temp file and
		// carry on appending there.
		f, err := os.CreateTemp("", "dimse_data_*")
		if err != nil {
			return err
		}
		if _, err := f.Write(dc.mem); err != nil {
			f.Close()
			os.Remove(f.Name())
			return err
		}
		dc.fpath, dc.w, dc.mem, dc.memPos = f.Name(), f, nil, 0
	}

	// A reader opened earlier would not see the new bytes' length reliably;
	// close it so the next read reopens (as the file-backed form always did).
	if dc.r != nil {
		dc.r.Close()
		dc.r = nil
	}
	if dc.w == nil {
		f, err := os.OpenFile(dc.fpath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return err
		}
		dc.w = f
	}
	_, err := dc.w.Write(data)
	return err
}

// Ack releases the data set: the memory, or the backing file (closed and
// removed). Safe to call more than once, and on a command never read.
func (dc *DimseCommand) Ack() error {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	dc.mem, dc.memPos = nil, 0
	dc.closeHandles()
	if dc.fpath == "" {
		return nil
	}
	err := os.Remove(dc.fpath)
	if os.IsNotExist(err) {
		err = nil
	}
	return err
}

// Close releases the read handle; the data set stays available until Ack.
func (dc *DimseCommand) Close() error {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	if dc.r != nil {
		err := dc.r.Close()
		dc.r = nil
		return err
	}
	return nil
}

func (dc *DimseCommand) closeHandles() {
	if dc.r != nil {
		dc.r.Close()
		dc.r = nil
	}
	if dc.w != nil {
		dc.w.Close()
		dc.w = nil
	}
}

// ReadData rewinds to the start of the data set and returns a reader for it,
// or nil if the backing file cannot be opened.
func (dc *DimseCommand) ReadData() io.Reader {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	if dc.fpath == "" {
		dc.memPos = 0
		return dc
	}
	if dc.r == nil {
		f, err := os.Open(dc.fpath)
		if err != nil {
			return nil
		}
		dc.r = f
	}
	if _, err := dc.r.Seek(0, io.SeekStart); err != nil {
		return nil
	}
	return dc.r
}

// Read implements io.Reader over the data set, from the current position.
func (dc *DimseCommand) Read(p []byte) (int, error) {
	dc.mu.Lock()
	if dc.fpath == "" {
		defer dc.mu.Unlock()
		if dc.memPos >= len(dc.mem) {
			return 0, io.EOF
		}
		n := copy(p, dc.mem[dc.memPos:])
		dc.memPos += n
		return n, nil
	}
	if dc.r == nil {
		f, err := os.Open(dc.fpath)
		if err != nil {
			dc.mu.Unlock()
			return 0, err
		}
		dc.r = f
	}
	r := dc.r
	dc.mu.Unlock()
	return r.Read(p)
}

// WriteTo implements io.WriterTo, so io.Copy from a buffered data set is one
// write of the whole buffer rather than a loop of small copies.
func (dc *DimseCommand) WriteTo(w io.Writer) (int64, error) {
	dc.mu.Lock()
	if dc.fpath == "" {
		rest := dc.mem[dc.memPos:]
		dc.memPos = len(dc.mem)
		dc.mu.Unlock()
		n, err := w.Write(rest)
		return int64(n), err
	}
	dc.mu.Unlock()
	// io.Copy would call WriteTo again; copy through a plain reader instead.
	return io.Copy(w, struct{ io.Reader }{dc})
}

// Bytes returns the whole data set. For a buffered data set this is the buffer
// itself, not a copy — valid until Ack, and the caller must not modify it.
func (dc *DimseCommand) Bytes() ([]byte, error) {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	if dc.fpath == "" {
		return dc.mem, nil
	}
	return os.ReadFile(dc.fpath)
}

// Size returns the data set's length in bytes, or -1 if it cannot be read.
func (dc *DimseCommand) Size() int64 {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	if dc.fpath == "" {
		return int64(len(dc.mem))
	}
	info, err := os.Stat(dc.fpath)
	if err != nil {
		return -1
	}
	return info.Size()
}

// InMemory reports whether the data set is currently held in memory — for
// tests and diagnostics.
func (dc *DimseCommand) InMemory() bool {
	dc.mu.Lock()
	defer dc.mu.Unlock()
	return dc.fpath == ""
}
