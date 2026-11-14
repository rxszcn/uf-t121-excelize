package excelize

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errZipWriter is a mock ZipWriter whose Create and Close can be configured to
// return errors.
type errZipWriter struct {
	createFunc func(string) (io.Writer, error)
	closeErr   error
}

func (m *errZipWriter) Create(name string) (io.Writer, error) {
	if m.createFunc != nil {
		return m.createFunc(name)
	}
	return &bytes.Buffer{}, nil
}

func (m *errZipWriter) AddFS(fs.FS) error { return nil }

func (m *errZipWriter) Close() error { return m.closeErr }

type errWriter struct{ err error }

func (e *errWriter) Write([]byte) (int, error) { return 0, e.err }

func BenchmarkWrite(b *testing.B) {
	const s = "This is test data"
	for b.Loop() {
		f := NewFile()
		for row := 1; row <= 10000; row++ {
			for col := 1; col <= 20; col++ {
				val, err := CoordinatesToCellName(col, row)
				if err != nil {
					b.Error(err)
				}
				if err := f.SetCellValue("Sheet1", val, s); err != nil {
					b.Error(err)
				}
			}
		}
		// Save spreadsheet by the given path.
		if err := f.SaveAs("test.xlsx"); err != nil {
			b.Error(err)
		}
	}
}

func TestWriteTo(t *testing.T) {
	// Test WriteToBuffer err
	{
		f, buf := File{Pkg: sync.Map{}}, bytes.Buffer{}
		f.SetZipWriter(func(w io.Writer) ZipWriter { return zip.NewWriter(w) })
		f.Pkg.Store("/d/", []byte("s"))
		_, err := f.WriteTo(bufio.NewWriter(&buf))
		assert.EqualError(t, err, "zip: write to directory")
		f.Pkg.Delete("/d/")
	}
	// Test file path overflow
	{
		f, buf := File{Pkg: sync.Map{}}, bytes.Buffer{}
		f.SetZipWriter(func(w io.Writer) ZipWriter { return zip.NewWriter(w) })
		const maxUint16 = 1<<16 - 1
		f.Pkg.Store(strings.Repeat("s", maxUint16+1), nil)
		_, err := f.WriteTo(bufio.NewWriter(&buf))
		assert.EqualError(t, err, "zip: FileHeader.Name too long")
	}
	// Test StreamsWriter err
	{
		f, buf := File{Pkg: sync.Map{}}, bytes.Buffer{}
		f.SetZipWriter(func(w io.Writer) ZipWriter { return zip.NewWriter(w) })
		f.Pkg.Store("s", nil)
		f.streams = make(map[string]*StreamWriter)
		file, _ := os.Open("123")
		f.streams["s"] = &StreamWriter{rawData: bufferedWriter{tmp: file}}
		_, err := f.WriteTo(bufio.NewWriter(&buf))
		assert.Nil(t, err)
	}
	// Test write with temporary file
	{
		f, buf := File{tempFiles: sync.Map{}}, bytes.Buffer{}
		f.SetZipWriter(func(w io.Writer) ZipWriter { return zip.NewWriter(w) })
		const maxUint16 = 1<<16 - 1
		f.tempFiles.Store("s", "")
		f.tempFiles.Store(strings.Repeat("s", maxUint16+1), "")
		_, err := f.WriteTo(bufio.NewWriter(&buf))
		assert.EqualError(t, err, "zip: FileHeader.Name too long")
	}
	// Test write with unsupported workbook file format
	{
		f, buf := File{Pkg: sync.Map{}}, bytes.Buffer{}
		f.SetZipWriter(func(w io.Writer) ZipWriter { return zip.NewWriter(w) })
		f.Pkg.Store("/d", []byte("s"))
		f.Path = "Book1.xls"
		_, err := f.WriteTo(bufio.NewWriter(&buf))
		assert.EqualError(t, err, ErrWorkbookFileFormat.Error())
	}
	// Test write with unsupported charset content types.
	{
		f, buf := NewFile(), bytes.Buffer{}
		f.ContentTypes, f.Path = nil, filepath.Join("test", "TestWriteTo.xlsx")
		f.Pkg.Store(defaultXMLPathContentTypes, MacintoshCyrillicCharset)
		_, err := f.WriteTo(bufio.NewWriter(&buf))
		assert.EqualError(t, err, "XML syntax error on line 1: invalid UTF-8")
	}
	// Test WriteToBuffer with ZipWriter Close error
	{
		f := NewFile()
		f.SetZipWriter(func(w io.Writer) ZipWriter {
			return &errZipWriter{closeErr: errors.New("close error")}
		})
		_, err := f.WriteTo(bufio.NewWriter(&bytes.Buffer{}))
		assert.EqualError(t, err, "close error")
	}
	// Test writeToZip with stream Create error
	{
		f := NewFile()
		f.streams = make(map[string]*StreamWriter)
		f.streams["s"] = &StreamWriter{rawData: bufferedWriter{}}
		f.SetZipWriter(func(w io.Writer) ZipWriter {
			return &errZipWriter{
				createFunc: func(name string) (io.Writer, error) {
					if name == "s" {
						return nil, errors.New("create stream error")
					}
					return &bytes.Buffer{}, nil
				},
			}
		})
		_, err := f.WriteTo(bufio.NewWriter(&bytes.Buffer{}))
		assert.EqualError(t, err, "create stream error")
	}
	// Test writeToZip with stream rawData.Reader() error
	{
		f := NewFile()
		f.streams = make(map[string]*StreamWriter)
		tmp, err := os.CreateTemp("", "excelize-test-*")
		assert.NoError(t, err)
		assert.NoError(t, tmp.Close())
		f.streams["s"] = &StreamWriter{rawData: bufferedWriter{tmp: tmp}}
		_, err = f.WriteTo(bufio.NewWriter(&bytes.Buffer{}))
		assert.Error(t, err)
	}
	// Test writeToZip with io.Copy error on stream
	{
		f := NewFile()
		f.streams = make(map[string]*StreamWriter)
		sw := &StreamWriter{}
		sw.rawData.buf.WriteString("test data")
		f.streams["s"] = sw
		f.SetZipWriter(func(w io.Writer) ZipWriter {
			return &errZipWriter{
				createFunc: func(name string) (io.Writer, error) {
					if name == "s" {
						return &errWriter{err: errors.New("copy error")}, nil
					}
					return &bytes.Buffer{}, nil
				},
			}
		})
		_, err := f.WriteTo(bufio.NewWriter(&bytes.Buffer{}))
		assert.EqualError(t, err, "copy error")
	}
}

// failingWriter is an io.Writer that always returns the configured error. It is
// handed out only for one specific archive entry; every other entry receives a
// normal in-memory writer.
type failingWriter struct {
	err error
}

func (w *failingWriter) Write([]byte) (int, error) { return 0, w.err }

// recordingZipWriter wraps errZipWriter and records the order in which Create
// is invoked, so tests can assert fail-fast behavior (no entry is created after
// the first failing entry).
type recordingZipWriter struct {
	*errZipWriter
	created []string
}

func (m *recordingZipWriter) Create(name string) (io.Writer, error) {
	m.created = append(m.created, name)
	return m.errZipWriter.Create(name)
}

// TestWriteToZipWriteErrorPropagation exercises the in-memory (Pkg) and
// on-disk (tempFiles) archive entry writers with fault injection. It pins down
// that an underlying write failure is surfaced as an explicit error instead of
// being silently swallowed, that the error is not overwritten by later
// successful writes, and that writing stops at the first failure.
func TestWriteToZipWriteErrorPropagation(t *testing.T) {
	// Entries are iterated in reverse-sorted order, so the lexicographically
	// larger "zzz-fail" is written first and "aaa-ok" afterwards. Under the old
	// code the successful write of "aaa-ok" reset the shared error to nil and the
	// corrupted archive was reported as success.
	const failEntry, okEntry = "zzz-fail", "aaa-ok"

	// newFaultingZipWriter returns a ZipWriter whose writer for failEntry always
	// fails, while every other entry is written successfully.
	newFaultingZipWriter := func(failErr error) *recordingZipWriter {
		return &recordingZipWriter{
			errZipWriter: &errZipWriter{
				createFunc: func(name string) (io.Writer, error) {
					if name == failEntry {
						return &failingWriter{err: failErr}, nil
					}
					return &bytes.Buffer{}, nil
				},
			},
		}
	}

	t.Run("pkg_write_error_is_not_overwritten", func(t *testing.T) {
		injected := errors.New("injected pkg write failure")
		f, buf := File{Pkg: sync.Map{}}, bytes.Buffer{}
		f.Pkg.Store(failEntry, []byte("failing entry"))
		f.Pkg.Store(okEntry, []byte("entry written after the failure"))
		zw := newFaultingZipWriter(injected)
		f.SetZipWriter(func(io.Writer) ZipWriter { return zw })

		_, err := f.WriteTo(&buf)
		assert.EqualError(t, err, injected.Error())
		// Fail-fast: the failing entry is created first and the entry that sorts
		// after it is never created nor written.
		assert.Equal(t, []string{failEntry}, zw.created)
	})

	t.Run("temp_file_write_error_is_not_overwritten", func(t *testing.T) {
		injected := errors.New("injected tempfile write failure")
		f, buf := File{tempFiles: sync.Map{}}, bytes.Buffer{}
		for _, path := range []string{failEntry, okEntry} {
			tmp, err := os.CreateTemp("", "excelize-fault-*")
			require.NoError(t, err)
			_, err = tmp.WriteString("temporary entry " + path)
			require.NoError(t, err)
			require.NoError(t, tmp.Close())
			f.tempFiles.Store(path, tmp.Name())
			t.Cleanup(func() { _ = os.Remove(tmp.Name()) })
		}
		zw := newFaultingZipWriter(injected)
		f.SetZipWriter(func(io.Writer) ZipWriter { return zw })

		_, err := f.WriteTo(&buf)
		assert.EqualError(t, err, injected.Error())
		// Fail-fast across the tempFiles loop as well.
		assert.Equal(t, []string{failEntry}, zw.created)
	})
}

// TestWriteToBufferSuccessArchiveIntact verifies the success-path artifact: the
// produced bytes must form a valid ZIP archive readable by archive/zip, every
// stored entry must round-trip byte-for-byte, and the workbook must open and be
// readable again downstream. This guards against regressions while the error
// propagation path was tightened.
func TestWriteToBufferSuccessArchiveIntact(t *testing.T) {
	f := NewFile()
	const cellValue = "success-path-payload"
	require.NoError(t, f.SetCellValue("Sheet1", "A1", cellValue))

	buf, err := f.WriteToBuffer()
	require.NoError(t, err)
	require.NotNil(t, buf)

	// The bytes form a valid ZIP archive and every entry round-trips exactly.
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	require.NoError(t, err)
	for _, entry := range zr.File {
		rc, err := entry.Open()
		require.NoError(t, err)
		got, err := io.ReadAll(rc)
		require.NoError(t, err)
		require.NoError(t, rc.Close())

		want, ok := f.Pkg.Load(entry.Name)
		require.True(t, ok, "unexpected archive entry %s", entry.Name)
		assert.Equal(t, want.([]byte), got, "entry %s changed on the success path", entry.Name)
	}
	assert.Len(t, zr.File, countPkgEntries(f))

	// Downstream can open the produced archive and read the cell back.
	reopened, err := OpenReader(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)
	got, err := reopened.GetCellValue("Sheet1", "A1")
	require.NoError(t, err)
	assert.Equal(t, cellValue, got)
	require.NoError(t, reopened.Close())
	require.NoError(t, f.Close())
}

// countPkgEntries returns the number of entries held in the in-memory package
// map, matching what writeToZip serializes on the success path.
func countPkgEntries(f *File) (count int) {
	f.Pkg.Range(func(_, _ interface{}) bool {
		count++
		return true
	})
	return
}

func TestClose(t *testing.T) {
	f := NewFile()
	f.tempFiles.Store("/d/", "/d/")
	require.Error(t, f.Close())
}

func TestZip64(t *testing.T) {
	f := NewFile()
	_, err := f.NewSheet("Sheet2")
	assert.NoError(t, err)
	sw, err := f.NewStreamWriter("Sheet1")
	assert.NoError(t, err)
	for r := range 131 {
		rowData := make([]interface{}, 1000)
		for c := range 1000 {
			rowData[c] = strings.Repeat("c", TotalCellChars)
		}
		cell, err := CoordinatesToCellName(1, r+1)
		assert.NoError(t, err)
		assert.NoError(t, sw.SetRow(cell, rowData))
	}
	assert.NoError(t, sw.Flush())
	assert.NoError(t, f.SaveAs(filepath.Join("test", "TestZip64.xlsx")))
	assert.NoError(t, f.Close())

	// Test with filename length overflow
	f = NewFile()
	f.zip64Entries = append(f.zip64Entries, defaultXMLPathSharedStrings)
	buf := new(bytes.Buffer)
	buf.Write([]byte{0x50, 0x4b, 0x03, 0x04})
	buf.Write(make([]byte, 20))
	assert.NoError(t, f.writeZip64LFH(buf))

	// Test with file header less than the required 30 for the fixed header part
	f = NewFile()
	f.zip64Entries = append(f.zip64Entries, defaultXMLPathSharedStrings)
	buf.Reset()
	buf.Write([]byte{0x50, 0x4b, 0x03, 0x04})
	buf.Write(make([]byte, 22))
	assert.NoError(t, binary.Write(buf, binary.LittleEndian, uint16(10)))
	buf.Write(make([]byte, 2))
	buf.WriteString("test")
	assert.NoError(t, f.writeZip64LFH(buf))

	t.Run("for_save_zip64_with_in_memory_file_over_4GB", func(t *testing.T) {
		// Test save workbook in ZIP64 format with in memory file with size over 4GB.
		f := NewFile()
		f.Sheet.Delete("xl/worksheets/sheet1.xml")
		f.Pkg.Store("xl/worksheets/sheet1.xml", make([]byte, math.MaxUint32+1))
		_, err := f.WriteToBuffer()
		assert.NoError(t, err)
		assert.NoError(t, f.Close())
	})

	t.Run("for_save_zip64_with_in_temporary_file_over_4GB", func(t *testing.T) {
		// Test save workbook in ZIP64 format with temporary file with size over 4GB.
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Skip()
		}
		f := NewFile()
		f.Pkg.Delete("xl/worksheets/sheet1.xml")
		f.Sheet.Delete("xl/worksheets/sheet1.xml")
		tmp, err := os.CreateTemp(os.TempDir(), "excelize-")
		assert.NoError(t, err)
		assert.NoError(t, tmp.Truncate(math.MaxUint32+1))
		f.tempFiles.Store("xl/worksheets/sheet1.xml", tmp.Name())
		assert.NoError(t, tmp.Close())
		_, err = f.WriteToBuffer()
		assert.NoError(t, err)
		assert.NoError(t, f.Close())
	})
}

func TestRemoveTempFiles(t *testing.T) {
	tmp, err := os.CreateTemp("", "excelize-*")
	if err != nil {
		t.Fatal(err)
	}
	tmpName := tmp.Name()
	assert.NoError(t, tmp.Close())
	f := NewFile()
	// Fill the tempFiles map with non-existing files
	for i := range 1000 {
		f.tempFiles.Store(strconv.Itoa(i), "/hopefully not existing")
	}
	f.tempFiles.Store("existing", tmpName)

	require.Error(t, f.Close())
	if _, err := os.Stat(tmpName); err == nil {
		t.Errorf("temp file %q still exist", tmpName)
		assert.NoError(t, os.Remove(tmpName))
	}
}
