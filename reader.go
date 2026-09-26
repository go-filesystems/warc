// SPDX-License-Identifier: BSD-3-Clause

package warc

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
)

// crlfcrlf is the two-CRLF record trailer. It is PART OF THE RECORD, not a
// separator between records: ISO 28500 §5 gives a record as version line, named
// fields, CRLF, exactly Content-Length octets of block, CRLF, CRLF. A reader
// that instead skips whitespace until the next "WARC/" accepts a truncated
// record as a whole one, and mis-frames the file whenever a block ends in a
// newline -- which for a response record holding an HTTP message is the common
// case, not the odd one.
var crlfcrlf = []byte("\r\n\r\n")

// Reader reads WARC records from a stream, one at a time, in file order.
//
// # What it does not do: gzip
//
// A .warc.gz is not a gzipped .warc. It is a concatenation of one gzip MEMBER
// per record, so that an index can seek to a record and inflate it alone; the
// deflate streams are per-record and the file is only valid gzip by the
// multi-member rule. This package handles PLAIN .warc and nothing else. Wrap
// the stream yourself for the single-member case:
//
//	zr, err := gzip.NewReader(f)   // and zr.Multistream(false) per member
//	r, err := warc.NewReader(zr)
//
// In this family it is not a gap in practice:
// github.com/go-filesystems/unarchive peels compression before it dispatches on
// format, so a .warc.gz reaches this package already inflated. It IS a gap for
// a caller that wants per-record random access into a .warc.gz, which needs the
// member offsets this package does not compute.
type Reader struct {
	br  *bufio.Reader
	err error

	// pos is the offset of the next unconsumed byte, counted by this package
	// as it consumes bytes out of br -- NOT by watching the underlying
	// reader, which bufio has already read ahead of. The filesystem view
	// needs a block's offset to read it later without holding it in memory,
	// so every consumption in this file goes through readLine, readN or
	// copyN and adds to pos.
	pos int64

	// n counts records already returned, so an error can say which record
	// was bad, and so the filesystem view can number its entries.
	n int

	// blockOff and blockLen describe the block of the record Next returned
	// last. The filesystem view reads them; nothing else does.
	blockOff int64
	blockLen int64
}

// NewReader returns a Reader that reads records from r.
//
// It returns an error only for a nil reader. The signature keeps the error
// because a caller writing `r, err := warc.NewReader(f)` should not have to
// change shape if a future version validates something up front, and because a
// nil io.Reader here would otherwise panic on the first Next instead of at the
// call that was wrong.
func NewReader(r io.Reader) (*Reader, error) {
	if r == nil {
		return nil, errors.New("warc: NewReader: nil reader")
	}
	return newReader(r), nil
}

// newReader is NewReader without the nil check, for callers inside the package
// that hold a reader they constructed themselves. Open uses it: passing its own
// *io.SectionReader through NewReader would leave that function's error return
// unreachable, and an error path no test can reach is one nobody has checked.
func newReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReader(r)}
}

// Next returns the next record, or io.EOF at a clean end of file -- that is,
// immediately after a record's second CRLF with nothing following.
//
// The returned Record owns its Block; the Reader keeps no reference to it, so a
// caller may retain records across calls.
//
// Errors are sticky. A malformed record leaves the stream at an offset this
// package cannot justify resuming from -- the whole point of Content-Length
// framing is that there is no resynchronisation point to scan for -- so every
// later Next returns the same error rather than pretending to recover.
func (r *Reader) Next() (*Record, error) {
	if r.err != nil {
		return nil, r.err
	}
	rec, err := r.next()
	if err != nil {
		r.err = err
		return nil, err
	}
	r.n++
	return rec, nil
}

// Records returned so far.
func (r *Reader) Records() int { return r.n }

func (r *Reader) next() (*Record, error) {
	version, err := r.readLine()
	if err != nil {
		// A clean end of file is the one place io.EOF is the answer
		// rather than a complaint: nothing of a record was consumed.
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, r.wrap("version line", err)
	}
	if version != Version10 && version != Version11 {
		return nil, r.errorf("unsupported version line %q (want %q or %q)", version, Version10, Version11)
	}

	hdr, err := r.readHeader()
	if err != nil {
		return nil, err
	}

	// Content-Length is mandatory and must be unambiguous. Two of them is not
	// a field we can pick between: one of the two lengths mis-frames every
	// record after this one, and there is no way to tell which.
	lengths := hdr.Values(HeaderContentLength)
	if len(lengths) != 1 {
		return nil, r.errorf("record has %d Content-Length fields, want exactly 1", len(lengths))
	}
	clen, err := parseContentLength(lengths[0])
	if err != nil {
		return nil, r.wrapSyntax(err)
	}

	if err := r.checkMandatory(hdr); err != nil {
		return nil, err
	}

	// The block is read by COUNT, never by looking for a delimiter, and into
	// a buffer that grows only as bytes actually arrive: Content-Length comes
	// from the file, so make([]byte, clen) would let a four-byte header line
	// ask for an arbitrary allocation.
	blockOff := r.pos
	var buf bytes.Buffer
	if err := r.copyN(&buf, clen); err != nil {
		return nil, r.wrap("block", err)
	}

	tail, err := r.readN(4)
	if err != nil {
		return nil, r.wrap("record trailer", err)
	}
	if !bytes.Equal(tail, crlfcrlf) {
		return nil, r.errorf("record trailer is %q, want CRLF CRLF", tail)
	}

	r.blockOff, r.blockLen = blockOff, clen
	return &Record{Version: version, Header: hdr, Block: buf.Bytes()}, nil
}

// readHeader reads named fields up to and including the empty line that ends
// the header block.
func (r *Reader) readHeader() (Header, error) {
	var hdr Header
	for {
		line, err := r.readLine()
		if err != nil {
			return nil, r.wrap("header", err)
		}
		if line == "" {
			return hdr, nil
		}
		// A line beginning with SP or HT continues the previous field's
		// value: ISO 28500 §4 carries RFC 2616's LWS folding into
		// field-value. Unfolding replaces the fold with one SP, which is
		// what the grammar says the value means -- so a folded value and
		// the same value on one line are the same value, and WriteRecord
		// writes it back on one line.
		if line[0] == ' ' || line[0] == '\t' {
			if len(hdr) == 0 {
				return nil, r.errorf("header block starts with a continuation line %q", line)
			}
			hdr[len(hdr)-1].Value = strings.TrimRight(hdr[len(hdr)-1].Value+" "+strings.Trim(line, " \t"), " \t")
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, r.errorf("header line %q has no colon", line)
		}
		// Cut splits at the FIRST colon, which is the only correct place:
		// a WARC-Target-URI value is a URI and contains one, and
		// WARC-Record-ID's value is a URN made of them.
		if !isToken(name) {
			return nil, r.errorf("header line %q has an invalid field name %q", line, name)
		}
		hdr = append(hdr, Field{Name: name, Value: strings.Trim(value, " \t")})
	}
}

// checkMandatory enforces §5's mandatory fields. WARC-Type is NOT checked
// against the eight known values: §5.5 requires a reader to pass an unknown
// record type through, so refusing one here would make this package reject
// files a conforming writer is allowed to produce.
func (r *Reader) checkMandatory(hdr Header) error {
	for _, name := range []string{HeaderType, HeaderDate, HeaderRecordID} {
		if !hdr.Has(name) {
			return r.errorf("record is missing mandatory field %s", name)
		}
	}
	id := hdr.Get(HeaderRecordID)
	if len(id) < 3 || id[0] != '<' || id[len(id)-1] != '>' {
		return r.errorf("WARC-Record-ID %q is not a URI in angle brackets", id)
	}
	if _, err := parseWARCDate(hdr.Get(HeaderDate)); err != nil {
		return r.wrapSyntax(err)
	}
	return nil
}

// readLine consumes one CRLF-terminated line and returns it without the CRLF.
//
// The terminator must be CRLF. A bare LF is refused, and so is a CR anywhere
// inside the line: both are octets the grammar does not allow there, and
// accepting either is how a reader ends up trimming whitespace instead of
// framing.
func (r *Reader) readLine() (string, error) {
	raw, err := r.br.ReadString('\n')
	r.pos += int64(len(raw))
	if err != nil {
		if errors.Is(err, io.EOF) && raw == "" {
			return "", io.EOF
		}
		if errors.Is(err, io.EOF) {
			return "", errorf("line %q is not CRLF-terminated at end of input", raw)
		}
		return "", err
	}
	if len(raw) < 2 || raw[len(raw)-2] != '\r' {
		return "", errorf("line %q is LF-terminated, want CRLF", raw)
	}
	line := raw[:len(raw)-2]
	if strings.IndexByte(line, '\r') >= 0 {
		return "", errorf("line %q contains a CR that does not end it", raw)
	}
	return line, nil
}

// readN consumes exactly n bytes.
func (r *Reader) readN(n int) ([]byte, error) {
	buf := make([]byte, n)
	got, err := io.ReadFull(r.br, buf)
	r.pos += int64(got)
	if err != nil {
		return nil, err
	}
	return buf, nil
}

// copyN consumes exactly n bytes into w. io.CopyN returns a nil error only when
// all n arrived, so there is no "short but fine" case to test for afterwards.
func (r *Reader) copyN(w io.Writer, n int64) error {
	got, err := io.CopyN(w, r.br, n)
	r.pos += got
	if errors.Is(err, io.EOF) {
		// io.CopyN says EOF for a source that ran out early; the caller
		// asked for a fixed count, so that is a truncated file.
		return io.ErrUnexpectedEOF
	}
	return err
}

// errorf reports a syntax complaint about the record currently being read. The
// record number goes in front of the caller's arguments, not into the format
// string it hands us, so a caller cannot get the two out of step.
func (r *Reader) errorf(format string, args ...any) error {
	return errorf("record %d: "+format, append([]any{r.n}, args...)...)
}

// wrap attaches the record number and what was being read to an error from a
// lower layer, keeping io.ErrUnexpectedEOF and real I/O errors distinguishable
// by errors.Is.
func (r *Reader) wrap(what string, err error) error {
	if errors.Is(err, io.EOF) {
		// End of input anywhere but at a record boundary is a truncated
		// file, and next() has already dealt with the one place a clean
		// io.EOF is the answer. Reporting io.EOF from the middle of a
		// record would tell a caller's `for` loop that the file ended
		// normally, which is how half a record gets accepted.
		err = io.ErrUnexpectedEOF
	}
	if errors.Is(err, ErrSyntax) {
		return r.errorf("%s: %s", what, err)
	}
	return &wrappedError{msg: fmt.Sprintf("warc: record %d: %s", r.n, what), err: err}
}

// wrapSyntax attaches the record number to a complaint raised by a helper that
// does not know it.
func (r *Reader) wrapSyntax(err error) error {
	return r.errorf("%s", strings.TrimPrefix(err.Error(), "warc: "))
}

type wrappedError struct {
	msg string
	err error
}

func (e *wrappedError) Error() string { return e.msg + ": " + e.err.Error() }
func (e *wrappedError) Unwrap() error { return e.err }
