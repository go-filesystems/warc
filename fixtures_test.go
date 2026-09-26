// SPDX-License-Identifier: BSD-3-Clause

package warc

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"testing"
)

// The two fixtures are EMBEDDED rather than read from testdata/ at run time,
// because the cross-architecture lanes of this repository's CI build a test
// binary with `go test -c` and run it inside a container that has the binary and
// nothing else. A test that calls os.ReadFile("testdata/x") passes on the four
// native lanes and fails on the four emulated ones, which is the worst of the
// two outcomes: the lane that is hardest to reproduce locally is the one that
// goes red.
//
// Where they come from, and what that is worth, is in doc.go and README.md.
// Briefly: handbuiltWARC is octets we laid out from ISO 28500 §5 by hand, and
// warcioWARC was written by warcio, an implementation we did not write.

//go:embed testdata/handbuilt.warc
var handbuiltWARC []byte

//go:embed testdata/warcio.warc
var warcioWARC []byte

// readAll reads every record, failing the test on any error other than EOF.
func readAll(t *testing.T, data []byte) []*Record {
	t.Helper()
	r, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	var out []*Record
	for {
		rec, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next after %d records: %v", len(out), err)
		}
		out = append(out, rec)
	}
	if r.Records() != len(out) {
		t.Errorf("Records() = %d, want %d", r.Records(), len(out))
	}
	return out
}

// errReader yields n bytes of data and then fails, so a test can reach the
// error paths that are neither a clean EOF nor a truncation.
type errReader struct {
	data []byte
	n    int
	err  error
}

func (e *errReader) Read(p []byte) (int, error) {
	if e.n <= 0 {
		return 0, e.err
	}
	if len(p) > e.n {
		p = p[:e.n]
	}
	c := copy(p, e.data)
	e.data = e.data[c:]
	e.n -= c
	return c, nil
}

// failWriter fails on the nth Write (1-based), so each of WriteRecord's three
// writes can be made to fail on its own.
type failWriter struct {
	n   int
	got int
	err error
}

func (w *failWriter) Write(p []byte) (int, error) {
	w.got++
	if w.got == w.n {
		return 0, w.err
	}
	return len(p), nil
}

// flakyReaderAt reads from a slice and starts failing once fail is set, so a
// test can build an FS from a good file and then make the block read fail.
type flakyReaderAt struct {
	data []byte
	fail bool
}

func (f *flakyReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if f.fail {
		return 0, errors.New("flaky: I/O error")
	}
	if off >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
