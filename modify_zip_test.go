package main

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// readZipEntries returns every entry's decompressed contents, by name.
func readZipEntries(t *testing.T, zr *zip.Reader) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", f.Name, err)
		}
		data, err := io.ReadAll(rc) // also verifies the CRC
		rc.Close()
		if err != nil {
			t.Fatalf("read %s: %v", f.Name, err)
		}
		out[f.Name] = data
	}
	return out
}

// Workers now encode and compress their own entries and only append them under
// the archive's lock. The archive must still hold exactly what a folder export
// writes, file for file — with already-compressed pixel data stored rather than
// deflated, and everything else deflated as before. Enough files that several
// workers really do append concurrently (run under -race too).
func TestZipExportMatchesFolderExport(t *testing.T) {
	rootDir := t.TempDir()
	var files []string
	for i := 0; i < 12; i++ {
		p := filepath.Join(rootDir, fmt.Sprintf("native%02d.dcm", i))
		writeRawPixelFixture(t, p, tsExplicitVRLE)
		files = append(files, p)
	}
	jpeg := filepath.Join(rootDir, "jpeg.dcm")
	writeEncapsulatedTestDICOM(t, jpeg, tsJPEGBaseline)
	files = append(files, jpeg)

	params, err := compileModifyParams(ModProfile{Sets: []string{"0010,0010=ANON"}})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	folder := t.TempDir()
	if res := runModification(context.Background(), files, rootDir, folder, params, nil, nil); res.Failed != 0 {
		t.Fatalf("folder export: %+v", res)
	}
	zipPath := filepath.Join(t.TempDir(), "export.zip")
	if res := runModificationToZip(context.Background(), files, rootDir, zipPath, params, nil, nil); res.Failed != 0 || res.Processed != len(files) {
		t.Fatalf("zip export: %+v", res)
	}

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer zr.Close()
	entries := readZipEntries(t, &zr.Reader)
	if len(entries) != len(files) {
		t.Fatalf("zip holds %d entries, want %d", len(entries), len(files))
	}
	for name, data := range entries {
		want, err := os.ReadFile(filepath.Join(folder, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("entry %s has no folder counterpart: %v", name, err)
		}
		if !bytes.Equal(data, want) {
			t.Errorf("entry %s differs from the folder export's file", name)
		}
	}
	for _, f := range zr.File {
		want := zip.Deflate
		if f.Name == "jpeg.dcm" {
			want = zip.Store
		}
		if f.Method != want {
			t.Errorf("entry %s method %d, want %d", f.Name, f.Method, want)
		}
	}
}

// An entry a worker compressed must carry the header CreateHeader would have
// given it — the UTF-8 name flag for a name outside CP-437's safe range, the
// version needed to extract — and the very same compressed bytes, since the
// worker deflates at archive/zip's own level. Checked against entries written
// through zw.Create, for an ASCII name and an accented one.
func TestZipSinkEntriesMatchCreate(t *testing.T) {
	payload := bytes.Repeat([]byte("DICOM header and pixels 0123456789 "), 500)
	names := []string{"DOE^JANE/CT Chest/1.dcm", "MÜLLER^HANS/Étude/1.dcm"}

	// Reference: the standard path.
	var ref bytes.Buffer
	rw := zip.NewWriter(&ref)
	for _, n := range names {
		w, err := rw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		w.Write(payload)
	}
	rw.Close()

	// The worker path.
	var got bytes.Buffer
	gw := zip.NewWriter(&got)
	sink := &zipSink{zw: gw}
	for _, n := range names {
		comp := deflateForTest(t, payload)
		if err := sink.appendRaw(zipRawHeader(n, zip.Deflate, crc32IEEE(payload), len(comp), len(payload)), comp); err != nil {
			t.Fatal(err)
		}
	}
	gw.Close()

	rr, _ := zip.NewReader(bytes.NewReader(ref.Bytes()), int64(ref.Len()))
	gr, err := zip.NewReader(bytes.NewReader(got.Bytes()), int64(got.Len()))
	if err != nil {
		t.Fatalf("worker-built archive does not open: %v", err)
	}
	for i := range names {
		r, g := rr.File[i], gr.File[i]
		if g.Name != r.Name || g.Method != r.Method || g.CRC32 != r.CRC32 ||
			g.CompressedSize64 != r.CompressedSize64 || g.UncompressedSize64 != r.UncompressedSize64 ||
			g.ReaderVersion != r.ReaderVersion || g.Flags&0x800 != r.Flags&0x800 {
			t.Errorf("entry %q header = %+v, want it to match %+v", names[i], g.FileHeader, r.FileHeader)
		}
		rc, _ := r.OpenRaw()
		gc, _ := g.OpenRaw()
		rb, _ := io.ReadAll(rc)
		gb, _ := io.ReadAll(gc)
		if !bytes.Equal(rb, gb) {
			t.Errorf("entry %q compressed bytes differ from zw.Create's", names[i])
		}
	}
	if gr.File[1].Flags&0x800 == 0 {
		t.Error("accented name written without the UTF-8 flag")
	}
	if contents := readZipEntries(t, gr); !bytes.Equal(contents[names[1]], payload) {
		t.Error("accented entry does not read back")
	}
}

// Above zipParallelMaxBytes an entry is deflated under the lock from its
// encoded bytes instead of into a second in-memory copy; the export must be
// unchanged by which path each entry took.
func TestZipExportLargeEntryPath(t *testing.T) {
	saved := zipParallelMaxBytes
	zipParallelMaxBytes = 16
	t.Cleanup(func() { zipParallelMaxBytes = saved })
	TestZipExportMatchesFolderExport(t)
}

func deflateForTest(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	fw, err := flate.NewWriter(&buf, zipDeflateLevel)
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(data)
	fw.Close()
	return buf.Bytes()
}

func crc32IEEE(data []byte) uint32 { return crc32.ChecksumIEEE(data) }
