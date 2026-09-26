// SPDX-License-Identifier: BSD-3-Clause

package warc

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The two WARC versions this package reads and writes. ISO 28500:2009 defined
// 1.0; ISO 28500:2017 defined 1.1, whose only framing-relevant change is that
// WARC-Date may carry fractional seconds. Anything else in the version line is
// refused rather than guessed at, because a reader that accepts a version it
// does not know cannot promise it framed the file correctly.
const (
	Version10 = "WARC/1.0"
	Version11 = "WARC/1.1"
)

// The WARC-Type values ISO 28500 defines. A reader must NOT refuse a type it
// does not know -- §5.5 says an unknown record type is to be skipped, not
// treated as a broken file -- so these are here for writers and for callers
// that switch on the type, and the reader never consults them.
const (
	TypeWarcinfo     = "warcinfo"
	TypeResponse     = "response"
	TypeRequest      = "request"
	TypeMetadata     = "metadata"
	TypeResource     = "resource"
	TypeRevisit      = "revisit"
	TypeConversion   = "conversion"
	TypeContinuation = "continuation"
)

// Named fields this package refers to by name. The four mandatory ones are
// listed first; the rest are here so callers do not have to spell them.
const (
	HeaderType          = "WARC-Type"
	HeaderDate          = "WARC-Date"
	HeaderRecordID      = "WARC-Record-ID"
	HeaderContentLength = "Content-Length"

	HeaderConcurrentTo   = "WARC-Concurrent-To"
	HeaderContentType    = "Content-Type"
	HeaderBlockDigest    = "WARC-Block-Digest"
	HeaderPayloadDigest  = "WARC-Payload-Digest"
	HeaderIPAddress      = "WARC-IP-Address"
	HeaderRefersTo       = "WARC-Refers-To"
	HeaderTargetURI      = "WARC-Target-URI"
	HeaderTruncated      = "WARC-Truncated"
	HeaderWarcinfoID     = "WARC-Warcinfo-ID"
	HeaderFilename       = "WARC-Filename"
	HeaderProfile        = "WARC-Profile"
	HeaderIdentifiedPLD  = "WARC-Identified-Payload-Type"
	HeaderSegmentNumber  = "WARC-Segment-Number"
	HeaderSegmentOriginI = "WARC-Segment-Origin-ID"
	HeaderSegmentTotal   = "WARC-Segment-Total-Length"
)

// ErrSyntax is the class of every failure to frame or parse a WARC file: a
// version line that is not one, a header line without a colon, a
// Content-Length that is not a decimal count of octets, a record whose two
// trailing CRLFs are not there. Callers distinguish "this is not a WARC" from
// an I/O failure with errors.Is(err, warc.ErrSyntax); the wrapped message says
// which record and what was wrong.
var ErrSyntax = errors.New("warc: syntax")

// ErrReadOnly is returned by every mutating method of the filesystem view. A
// WARC file is an append-only log of what a crawl saw; rewriting a record in
// place would change a record of fact, so the Filesystem surface refuses it
// rather than implementing half of it.
var ErrReadOnly = errors.New("warc: filesystem is read-only")

// Field is one WARC named field: a name and its value, in the order the record
// carries them.
//
// Fields are a SLICE and not a map because two properties of the format make a
// map wrong. A WARC record may repeat a field name -- WARC-Concurrent-To is the
// ordinary case, a record concurrent with several others -- and a map either
// loses the repeats or has to model them anyway. And a WARC file is evidence: a
// reader that reorders the header block cannot hand a caller the bytes it read,
// so a digest computed over a re-serialised header would not match the one the
// crawler wrote.
type Field struct {
	Name  string
	Value string
}

// Header is a record's named fields, in file order.
type Header []Field

// Get returns the value of the first field with this name, comparing names
// case-insensitively as the grammar's token production requires, or "" if
// there is none. Use Values when a name may repeat: Get cannot tell one
// absent field from one whose value is empty, and Has can.
func (h Header) Get(name string) string {
	for _, f := range h {
		if strings.EqualFold(f.Name, name) {
			return f.Value
		}
	}
	return ""
}

// Values returns every value carried under this name, in file order, or nil if
// there is none.
func (h Header) Values(name string) []string {
	var out []string
	for _, f := range h {
		if strings.EqualFold(f.Name, name) {
			out = append(out, f.Value)
		}
	}
	return out
}

// Has reports whether the name is present at all, which is not the same
// question as Get returning a non-empty string.
func (h Header) Has(name string) bool {
	for _, f := range h {
		if strings.EqualFold(f.Name, name) {
			return true
		}
	}
	return false
}

// Add appends a field, keeping any field of the same name that is already
// there. This is the call for WARC-Concurrent-To and the other repeatable
// fields.
func (h *Header) Add(name, value string) {
	*h = append(*h, Field{Name: name, Value: value})
}

// Set replaces the value of the first field with this name, in place so the
// field keeps its position, and deletes any further field of the same name. If
// the name is absent it is appended.
func (h *Header) Set(name, value string) {
	out := (*h)[:0]
	seen := false
	for _, f := range *h {
		if !strings.EqualFold(f.Name, name) {
			out = append(out, f)
			continue
		}
		if seen {
			continue
		}
		seen = true
		out = append(out, Field{Name: f.Name, Value: value})
	}
	if !seen {
		out = append(out, Field{Name: name, Value: value})
	}
	*h = out
}

// Del removes every field with this name.
func (h *Header) Del(name string) {
	out := (*h)[:0]
	for _, f := range *h {
		if !strings.EqualFold(f.Name, name) {
			out = append(out, f)
		}
	}
	*h = out
}

// Clone returns a copy that shares no storage with h, so a caller can keep a
// record's header after handing the record on.
func (h Header) Clone() Header {
	if h == nil {
		return nil
	}
	out := make(Header, len(h))
	copy(out, h)
	return out
}

// Record is one WARC record: its version line, its named fields in file order,
// and its content block.
//
// Block is the block EXACTLY as the file carries it, Content-Length octets of
// it, with nothing trimmed. The block is opaque: a response record's block is
// an HTTP message, a warcinfo block is an application/warc-fields document, a
// resource block is whatever was fetched, and any of them may contain CRLF,
// NUL or bytes that are not text at all. Nothing in this package looks inside
// it.
type Record struct {
	// Version is the version line, e.g. Version11. NewWriter's WriteRecord
	// treats "" as Version11.
	Version string
	// Header is the named fields, in order.
	Header Header
	// Block is the content block, Content-Length octets exactly.
	Block []byte
}

// Type returns WARC-Type, or "" if the field is absent. The reader guarantees
// it is present, so "" means the Record was built by a caller.
func (r *Record) Type() string { return r.Header.Get(HeaderType) }

// RecordID returns WARC-Record-ID as the file carries it, angle brackets and
// all: the field's value is a URI in angle brackets, and stripping them here
// would make the value we hand back differ from the value we read.
func (r *Record) RecordID() string { return r.Header.Get(HeaderRecordID) }

// TargetURI returns WARC-Target-URI, or "" for the record types that have no
// target (warcinfo, and metadata about the archive itself).
func (r *Record) TargetURI() string { return r.Header.Get(HeaderTargetURI) }

// ContentType returns the block's Content-Type, or "" if unstated.
func (r *Record) ContentType() string { return r.Header.Get(HeaderContentType) }

// Date parses WARC-Date. The reader has already checked it parses, so the
// error here is for a Record a caller built.
func (r *Record) Date() (time.Time, error) {
	return parseWARCDate(r.Header.Get(HeaderDate))
}

// parseWARCDate accepts the UTC "W3C-DTF"/RFC 3339 form the spec mandates and
// nothing else. §5.4 requires the instant be expressed in UTC, so a value with
// a numeric offset -- even +00:00, which denotes the same instant -- is
// refused: accepting it would let a file through whose other tooling may read
// the offset differently, and a WARC-Date is the timestamp a crawl is dated by.
// WARC 1.1 permits fractional seconds and 1.0 does not; both are accepted,
// because refusing a 1.1 date in a file that says WARC/1.0 would reject files
// every 1.1-era tool writes and buys nothing.
func parseWARCDate(s string) (time.Time, error) {
	if !strings.HasSuffix(s, "Z") {
		return time.Time{}, errorf("WARC-Date %q is not a UTC W3C-DTF timestamp", s)
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, errorf("WARC-Date %q: %s", s, err)
	}
	return t, nil
}

// errorf builds an ErrSyntax. Every framing and parsing complaint in this
// package goes through here, so errors.Is(err, ErrSyntax) holds for all of
// them without each call site having to remember the %w.
func errorf(format string, args ...any) error {
	return &syntaxError{msg: "warc: " + fmt.Sprintf(format, args...)}
}

type syntaxError struct{ msg string }

func (e *syntaxError) Error() string { return e.msg }
func (e *syntaxError) Is(target error) bool {
	return target == ErrSyntax
}

// isDecimal reports whether s is a non-empty run of ASCII digits and nothing
// else. Content-Length is a count of octets, so it is exactly that: no sign, no
// whitespace, no underscores. strconv.ParseInt would accept "+5" and "-0",
// which are not counts.
func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// parseContentLength parses a mandatory Content-Length: a decimal count of
// octets, which must fit in an int64 because that is what every offset in this
// package is.
func parseContentLength(s string) (int64, error) {
	if !isDecimal(s) {
		return 0, errorf("Content-Length %q is not a decimal octet count", s)
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, errorf("Content-Length %q: %s", s, err)
	}
	return n, nil
}

// isToken reports whether s is a field-name: a non-empty run of the characters
// RFC 2616's token production allows, which is what ISO 28500 §4 points at.
// The two that matter are SP and COLON: a name containing either would make the
// header line we write parse as something else when read back.
func isToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= 0x20 || c >= 0x7f {
			return false
		}
		if strings.IndexByte("()<>@,;:\\\"/[]?={}", c) >= 0 {
			return false
		}
	}
	return true
}
