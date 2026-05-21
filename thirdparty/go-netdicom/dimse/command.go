package dimse

import (
	"io"
	"os"
	"sync"
	"sync/atomic"
)

type DimseCommand struct {
	fpath      string
	mu         sync.RWMutex
	dataReader *os.File
	sizeCache  int64 // cached size (-1 if unknown)
}

func NewDimseCommand(fpath string) *DimseCommand {
	return &DimseCommand{fpath: fpath}
}

func (dc *DimseCommand) AppendData(data []byte) error {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	// Close dataReader if it's open to prevent concurrent access
	if dc.dataReader != nil {
		dc.dataReader.Close()
		dc.dataReader = nil
	}

	// Open file in append mode, create if doesn't exist
	file, err := os.OpenFile(dc.fpath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer file.Close()

	// Write data to file
	_, err = file.Write(data)
	return err
}

func (dc *DimseCommand) Ack() error {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	err := dc.dataReader.Close()
	if err != nil {
		return err
	}

	return os.Remove(dc.fpath)
}

func (dc *DimseCommand) Close() error {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	if dc.dataReader != nil {
		return dc.dataReader.Close()
	}

	return nil
}

func (dc *DimseCommand) ReadData() io.Reader {
	dc.mu.Lock()
	defer dc.mu.Unlock()

	if dc.dataReader == nil {
		f, err := os.Open(dc.fpath)
		if err != nil {
			return nil
		}
		dc.dataReader = f
	}
	// Reset cursor for a fresh read.
	if _, err := dc.dataReader.Seek(0, io.SeekStart); err != nil {
		return nil
	}
	return dc.dataReader
}

// Read implements io.Reader by delegating to an internal *os.File reader.
func (dc *DimseCommand) Read(p []byte) (int, error) {
	dc.mu.Lock()
	if dc.dataReader == nil {
		f, err := os.Open(dc.fpath)
		if err != nil {
			dc.mu.Unlock()
			return 0, err
		}
		dc.dataReader = f
	}
	r := dc.dataReader
	dc.mu.Unlock()
	return r.Read(p)
}

// Size returns the current size of the underlying file. Returns -1 on error.
func (dc *DimseCommand) Size() int64 {
	if v := atomic.LoadInt64(&dc.sizeCache); v > 0 {
		return v
	}
	info, err := os.Stat(dc.fpath)
	if err != nil {
		return -1
	}
	atomic.StoreInt64(&dc.sizeCache, info.Size())
	return info.Size()
}
