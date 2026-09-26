// SPDX-License-Identifier: BSD-3-Clause

package warc

import (
	"bytes"
	"io"
	"strconv"
	"strings"
)

// Writer writes WARC records to a stream.
//
// It writes PLAIN .warc. A .warc.gz is one gzip member per record, which a
// caller composes by wrapping each WriteRecord in its own gzip.Writer and
// closing it before the next; this package does not do it, because doing it
// halfway -- one gzip stream around the whole file -- produces a file that
// inflates to a valid WARC and is NOT a valid .warc.gz, since no index can seek
// into it.
type Writer struct {
	w   io.Writer
	err error
	n   int
}

// NewWriter returns a Writer that writes records to w. It does not buffer: each
// WriteRecord issues three Writes (header block, content block, trailer), so a
// caller writing many small records should hand it a bufio.Writer.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w}
}

// Records written so far.
func (w *Writer) Records() int { return w.n }

// WriteRecord writes one record: version line, named fields, CRLF, the block's
// octets, CRLF, CRLF.
//
// It REFUSES rather than repairs, in four cases, because each one is a record
// that would read back as something other than what the caller passed:
//
//   - a missing WARC-Type, WARC-Date or WARC-Record-ID: §5 makes them
//     mandatory, and this package's own Reader would refuse the result.
//   - a field name that is not a token, or a value containing CR, LF or NUL: a
//     value with a CRLF in it does not produce a long header, it produces two,
//     and the second is whatever the caller's data happened to say. That is the
//     header-injection shape, and a writer is the only place it can be stopped.
//   - a value with leading or trailing space or tab: the grammar lets a reader
//     strip it, so writing it would silently change the value. Trim it first
//     and the round trip is exact.
//   - a Content-Length that disagrees with len(Block): one of the two is wrong
//     and the writer cannot know which. A record with no Content-Length gets
//     one, computed from the block, appended after the caller's fields.
//
// Errors are sticky: after a failed write the stream's contents are no longer a
// WARC, so further records are not appended to it.
func (w *Writer) WriteRecord(rec *Record) error {
	if w.err != nil {
		return w.err
	}
	if err := w.writeRecord(rec); err != nil {
		w.err = err
		return err
	}
	w.n++
	return nil
}

func (w *Writer) writeRecord(rec *Record) error {
	version := rec.Version
	if version == "" {
		version = Version11
	}
	if version != Version10 && version != Version11 {
		return errorf("WriteRecord: unsupported version %q (want %q or %q)", version, Version10, Version11)
	}

	for _, name := range []string{HeaderType, HeaderDate, HeaderRecordID} {
		if !rec.Header.Has(name) {
			return errorf("WriteRecord: missing mandatory field %s", name)
		}
	}
	if _, err := parseWARCDate(rec.Header.Get(HeaderDate)); err != nil {
		return err
	}
	id := rec.Header.Get(HeaderRecordID)
	if len(id) < 3 || id[0] != '<' || id[len(id)-1] != '>' {
		return errorf("WriteRecord: WARC-Record-ID %q is not a URI in angle brackets", id)
	}

	var head bytes.Buffer
	head.WriteString(version)
	head.WriteString("\r\n")
	haveLength := false
	for _, f := range rec.Header {
		if !isToken(f.Name) {
			return errorf("WriteRecord: %q is not a valid field name", f.Name)
		}
		if i := strings.IndexAny(f.Value, "\r\n\x00"); i >= 0 {
			return errorf("WriteRecord: value of %s contains %q at offset %d", f.Name, f.Value[i], i)
		}
		if trimmed := strings.Trim(f.Value, " \t"); trimmed != f.Value {
			return errorf("WriteRecord: value of %s has leading or trailing whitespace (%q)", f.Name, f.Value)
		}
		if strings.EqualFold(f.Name, HeaderContentLength) {
			if haveLength {
				return errorf("WriteRecord: two Content-Length fields")
			}
			haveLength = true
			want, err := parseContentLength(f.Value)
			if err != nil {
				return err
			}
			if want != int64(len(rec.Block)) {
				return errorf("WriteRecord: Content-Length says %d, block is %d octets", want, len(rec.Block))
			}
		}
		head.WriteString(f.Name)
		head.WriteString(": ")
		head.WriteString(f.Value)
		head.WriteString("\r\n")
	}
	if !haveLength {
		head.WriteString(HeaderContentLength)
		head.WriteString(": ")
		head.WriteString(strconv.Itoa(len(rec.Block)))
		head.WriteString("\r\n")
	}
	head.WriteString("\r\n")

	if _, err := w.w.Write(head.Bytes()); err != nil {
		return err
	}
	if _, err := w.w.Write(rec.Block); err != nil {
		return err
	}
	// The trailer is written even for a zero-length block: the two CRLFs are
	// part of the record, so a record with no block is still six octets of
	// framing after its header.
	if _, err := w.w.Write(crlfcrlf); err != nil {
		return err
	}
	return nil
}
