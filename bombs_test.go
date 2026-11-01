// SPDX-License-Identifier: BSD-3-Clause

package warc

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// Bombs: small inputs that made the reader allocate without bound, each measured
// before it was fixed (runtime.MemStats TotalAlloc around the call):
//
//	50 000 one-byte folds, 200 KB      2 655 393 224 bytes allocated  (13 000x)
//	one 256 MiB header line, no \n     2 424 052 328 bytes allocated
//	Open of one 64 MiB block             268 442 712 bytes allocated
//
// and after: 833 KB, 5.2 MB, 7.6 KB.

// allocated runs f and returns the bytes the runtime allocated meanwhile.
func allocated(f func()) uint64 {
	var a, b runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

// endless yields n copies of 'a' without ever holding them.
type endless struct{ n int64 }

func (e *endless) Read(p []byte) (int, error) {
	if e.n <= 0 {
		return 0, io.EOF
	}
	m := min(int64(len(p)), e.n)
	for i := range p[:m] {
		p[i] = 'a'
	}
	e.n -= m
	return int(m), nil
}

const bombHead = "WARC/1.1\r\nWARC-Type: metadata\r\nWARC-Record-ID: <urn:uuid:1>\r\n" +
	"WARC-Date: 2026-09-26T06:00:00Z\r\n"

func TestFoldingIsLinear(t *testing.T) {
	var in bytes.Buffer
	in.WriteString(bombHead + "X-Long: v\r\n")
	for range 50000 {
		in.WriteString(" x\r\n")
	}
	in.WriteString("Content-Length: 0\r\n\r\n\r\n\r\n")
	var rec *Record
	var err error
	got := allocated(func() {
		r, _ := NewReader(bytes.NewReader(in.Bytes()))
		rec, err = r.Next()
	})
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if want := "v" + strings.Repeat(" x", 50000); rec.Header.Get("X-Long") != want {
		t.Errorf("folded value is wrong (%d bytes, want %d)", len(rec.Header.Get("X-Long")), len(want))
	}
	// Measured 833 KB; the quadratic version was 2.6 GB.
	if got > 4<<20 {
		t.Errorf("%d bytes allocated for %d bytes of folds", got, in.Len())
	}
}

// TestFoldingKeepsItsMeaning pins the unfolding rule the linear version had to
// preserve: one SP per fold, outer whitespace trimmed, a blank fold adds nothing.
func TestFoldingKeepsItsMeaning(t *testing.T) {
	for _, tc := range []struct{ name, lines, want string }{
		{"plain", "A: a\r\n b\r\n", "a b"},
		{"tabs", "A: a\t\r\n\t b \t\r\n", "a b"},
		{"blank fold", "A: a\r\n \t\r\n b\r\n", "a b"},
		{"trailing blank fold", "A: a\r\n  \r\n", "a"},
		{"empty value", "A:\r\n b\r\n", " b"},
		{"empty everything", "A:\r\n \r\n", ""},
		{"two fields", "A: a\r\n b\r\nB: c\r\n d\r\n", "a b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := bombHead + tc.lines + "Content-Length: 0\r\n\r\n\r\n\r\n"
			r, _ := NewReader(strings.NewReader(in))
			rec, err := r.Next()
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			if got := rec.Header.Get("A"); got != tc.want {
				t.Errorf("A = %q, want %q", got, tc.want)
			}
		})
	}
	// The second field of the last case keeps its own fold.
	in := bombHead + "A: a\r\n b\r\nB: c\r\n d\r\nContent-Length: 0\r\n\r\n\r\n\r\n"
	r, _ := NewReader(strings.NewReader(in))
	rec, _ := r.Next()
	if got := rec.Header.Get("B"); got != "c d" {
		t.Errorf("B = %q, want %q", got, "c d")
	}
}

func TestAHeaderLineWithoutANewlineIsBounded(t *testing.T) {
	var err error
	got := allocated(func() {
		r, _ := NewReader(io.MultiReader(strings.NewReader(bombHead+"X: "), &endless{256 << 20}))
		_, err = r.Next()
	})
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Next = %v, want ErrTooLarge", err)
	}
	if errors.Is(err, ErrSyntax) {
		t.Errorf("a size refusal was reported as a syntax error: %v", err)
	}
	// Measured 5.2 MB (the 1 MiB budget, doubled by append, plus bufio's buffer).
	if got > 16<<20 {
		t.Errorf("%d bytes allocated before refusing", got)
	}
	// The same through the version line, which is the first line read.
	r, _ := NewReader(&endless{256 << 20})
	if _, err := r.Next(); !errors.Is(err, ErrTooLarge) {
		t.Errorf("an endless version line: %v, want ErrTooLarge", err)
	}
}

// padTo builds a header block whose total length is exactly n bytes.
func padTo(n int) string {
	const tail = "Content-Length: 0\r\n\r\n"
	head := bombHead + "X-Pad: "
	pad := n - len(head) - len("\r\n") - len(tail)
	return head + strings.Repeat("p", pad) + "\r\n" + tail
}

func TestTheHeaderCeilingIsExact(t *testing.T) {
	ok := padTo(MaxHeaderBytes) + "\r\n\r\n"
	r, _ := NewReader(strings.NewReader(ok))
	if _, err := r.Next(); err != nil {
		t.Fatalf("a header block of exactly MaxHeaderBytes was refused: %v", err)
	}
	over := padTo(MaxHeaderBytes+1) + "\r\n\r\n"
	r, _ = NewReader(strings.NewReader(over))
	if _, err := r.Next(); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("one byte over: %v, want ErrTooLarge", err)
	}
	// The budget is per record: a second record starts afresh.
	two := ok + ok
	r, _ = NewReader(strings.NewReader(two))
	for i := range 2 {
		if _, err := r.Next(); err != nil {
			t.Fatalf("record %d of two at the ceiling: %v", i, err)
		}
	}
}

// TestALongLineUnderTheCeilingIsKeptWhole crosses bufio's 4096-byte buffer, so the
// line is assembled from several pieces, and one that ends the input in the middle
// of such a line.
func TestALongLineUnderTheCeilingIsKeptWhole(t *testing.T) {
	val := strings.Repeat("v", 20000)
	in := bombHead + "X-Long: " + val + "\r\nContent-Length: 0\r\n\r\n\r\n\r\n"
	r, _ := NewReader(strings.NewReader(in))
	rec, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if rec.Header.Get("X-Long") != val {
		t.Errorf("the long value came back as %d bytes, want %d", len(rec.Header.Get("X-Long")), len(val))
	}
	cut := bombHead + "X-Long: " + val
	r, _ = NewReader(strings.NewReader(cut))
	_, err = r.Next()
	if !errors.Is(err, ErrSyntax) || !strings.Contains(err.Error(), "not CRLF-terminated") {
		t.Errorf("a long line cut by the end of input: %v", err)
	}
	if len(err.Error()) > 1024 {
		t.Errorf("the error is %d bytes long", len(err.Error()))
	}
}

func TestErrorsDoNotCarryTheWholeLine(t *testing.T) {
	big := strings.Repeat("z", 100000)
	for name, in := range map[string]string{
		"version":        big + "\r\n",
		"no colon":       bombHead + big + "\r\n",
		"bad name":       bombHead + big + ": v\r\n",
		"continuation":   "WARC/1.1\r\n " + big + "\r\n",
		"LF only":        bombHead + "X: " + big + "\n",
		"CR inside":      bombHead + "X: a\r" + big + "\r\n",
		"record id":      strings.Replace(bombHead, "<urn:uuid:1>", big, 1) + "Content-Length: 0\r\n\r\n",
		"date":           strings.Replace(bombHead, "2026-09-26T06:00:00Z", big+"Z", 1) + "Content-Length: 0\r\n\r\n",
		"content length": bombHead + "Content-Length: " + strings.Repeat("9", 100000) + "\r\n\r\n",
		"length range":   bombHead + "Content-Length: 99999999999999999999\r\n\r\n",
		"not a number":   bombHead + "Content-Length: " + big + "\r\n\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			r, _ := NewReader(strings.NewReader(in))
			_, err := r.Next()
			if err == nil {
				t.Fatal("accepted")
			}
			if len(err.Error()) > 1024 {
				t.Errorf("the error is %d bytes long", len(err.Error()))
			}
		})
	}
	if got := shown("abc"); got != "abc" {
		t.Errorf("shown(abc) = %q", got)
	}
}

func TestTheBlockCeilingOfAReaderIsExact(t *testing.T) {
	rec := func(n int) string {
		return bombHead + "Content-Length: " + strconv.Itoa(n) + "\r\n\r\n" + strings.Repeat("b", n) + "\r\n\r\n"
	}
	r, _ := NewReader(strings.NewReader(rec(10)))
	r.MaxBlockSize = 10
	if got, err := r.Next(); err != nil || len(got.Block) != 10 {
		t.Fatalf("a block at the ceiling: %v", err)
	}
	r, _ = NewReader(strings.NewReader(rec(11)))
	r.MaxBlockSize = 10
	var err error
	got := allocated(func() { _, err = r.Next() })
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("a block one over: %v, want ErrTooLarge", err)
	}
	if got > 64<<10 {
		t.Errorf("refusing the block allocated %d bytes", got)
	}
	// The default applies when the field is zero, one over it is refused, and a
	// claim at it on a short stream is a truncated file, not a refusal.
	in := bombHead + "Content-Length: " + strconv.Itoa(DefaultMaxBlockSize+1) + "\r\n\r\nhi\r\n\r\n"
	r, _ = NewReader(strings.NewReader(in))
	if _, err := r.Next(); !errors.Is(err, ErrTooLarge) {
		t.Errorf("default ceiling + 1: %v, want ErrTooLarge", err)
	}
}

// sparse is a ReaderAt of a huge file whose only content is a header at the start
// and the record trailer at the end, with 'a' between: reading it is free, holding
// it would not be.
type sparse struct {
	head, tail []byte
	size       int64
	reads      int64
}

func (s *sparse) ReadAt(p []byte, off int64) (int, error) {
	if off >= s.size {
		return 0, io.EOF
	}
	n := min(int64(len(p)), s.size-off)
	for i := int64(0); i < n; i++ {
		pos := off + i
		switch {
		case pos < int64(len(s.head)):
			p[i] = s.head[pos]
		case pos >= s.size-int64(len(s.tail)):
			p[i] = s.tail[pos-(s.size-int64(len(s.tail)))]
		default:
			p[i] = 'a'
		}
	}
	s.reads += n
	if n < int64(len(p)) {
		return int(n), io.EOF
	}
	return int(n), nil
}

func newSparse(blockLen int64) *sparse {
	head := bombHead + "Content-Length: " + strconv.FormatInt(blockLen, 10) + "\r\n\r\n"
	return &sparse{head: []byte(head), tail: []byte("\r\n\r\n"), size: int64(len(head)) + blockLen + 4}
}

func TestOpenDoesNotReadBlocks(t *testing.T) {
	const block = 3 << 30 // over DefaultMaxBlockSize, and over 2 GiB for a 32-bit int
	s := newSparse(block)
	var fsys *FS
	var err error
	got := allocated(func() { fsys, err = Open(s, s.size) })
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got > 1<<20 {
		t.Errorf("Open allocated %d bytes for a file of %d", got, s.size)
	}
	if s.reads > 1<<20 {
		t.Errorf("Open read %d bytes of a %d-byte file: the block was not stepped over", s.reads, s.size)
	}
	name := fsys.Names()[0]
	if _, err := fsys.ReadFile(name); !errors.Is(err, ErrTooLarge) {
		t.Errorf("ReadFile of a 3 GiB block: %v, want ErrTooLarge", err)
	}
	if _, err := fsys.Record(name); !errors.Is(err, ErrTooLarge) {
		t.Errorf("Record of a 3 GiB block: %v, want ErrTooLarge", err)
	}
	// OpenFile has no ceiling: it reads when asked.
	f, err := fsys.OpenFile(name)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if f.Size() != block {
		t.Errorf("Size = %d, want %d", f.Size(), int64(block))
	}
	buf := make([]byte, 4)
	if _, err := f.ReadAt(buf, block-4); err != nil || string(buf) != "aaaa" {
		t.Errorf("ReadAt near the end: %q, %v", buf, err)
	}
}

// TestOpenRefusesABlockPastTheEndWithoutReadingIt: the claim is checked against
// the section by subtraction, so one near the top of an int64 cannot overflow it.
func TestOpenRefusesABlockPastTheEndWithoutReadingIt(t *testing.T) {
	for _, n := range []string{"9223372036854775807", "9000000000000000000", "1000000"} {
		in := bombHead + "Content-Length: " + n + "\r\n\r\nhi\r\n\r\n"
		var err error
		got := allocated(func() { _, err = Open(strings.NewReader(in), int64(len(in))) })
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("Content-Length %s: Open = %v, want io.ErrUnexpectedEOF", n, err)
		}
		if got > 64<<10 {
			t.Errorf("Content-Length %s: %d bytes allocated", n, got)
		}
	}
}

// TestOpenAndNextAgree: stepping over a block (buffered discard or seek) must land
// where reading it would have, for blocks on either side of bufio's buffer.
func TestOpenAndNextAgree(t *testing.T) {
	var in bytes.Buffer
	for _, n := range []int{0, 1, 100, 4095, 4096, 4097, 10000, 70000, 3} {
		fmt.Fprintf(&in, "%sContent-Length: %d\r\n\r\n%s\r\n\r\n", bombHead, n, strings.Repeat("b", n))
	}
	b := in.Bytes()
	fsys, err := Open(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r, _ := NewReader(bytes.NewReader(b))
	for _, name := range fsys.Names() {
		rec, err := r.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		got, err := fsys.ReadFile(name)
		if err != nil || !bytes.Equal(got, rec.Block) {
			t.Fatalf("%s: ReadFile (%d bytes, %v) differs from Next (%d bytes)", name, len(got), err, len(rec.Block))
		}
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("after the last record: %v", err)
	}
}

// TestManyRecordsCostBoundedMemory: the entries Open keeps are the one allocation
// that grows with the file. The smallest record is the worst case.
func TestManyRecordsCostBoundedMemory(t *testing.T) {
	one := bombHead + "Content-Length: 0\r\n\r\n\r\n\r\n"
	file := []byte(strings.Repeat(one, (4<<20)/len(one)))
	var fsys *FS
	var err error
	got := allocated(func() { fsys, err = Open(bytes.NewReader(file), int64(len(file))) })
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Logf("%d bytes in, %d records, %d allocated (%.1fx)", len(file), len(fsys.Names()), got,
		float64(got)/float64(len(file)))
	// Measured at the time of writing; see the log line. A header is kept per
	// record, so this is a small multiple of the input, not a function of anything
	// the file can declare.
	if got > 24*uint64(len(file)) {
		t.Errorf("%d bytes allocated for %d bytes of records", got, len(file))
	}
}
