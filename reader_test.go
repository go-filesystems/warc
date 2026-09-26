// SPDX-License-Identifier: BSD-3-Clause

package warc

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// wantRecord is the whole expected record: version, every field in order, and
// the block's exact octets. Counting records is not a test -- a reader that
// mis-frames one record into two, or that trims the end of a block, gets the
// count right -- so every case here asserts the bytes and the field list.
type wantRecord struct {
	version string
	fields  []Field
	block   []byte
}

// handbuiltWant is written out from testdata/gen_handbuilt.py, which was itself
// written from ISO 28500 §5 rather than from this package.
var handbuiltWant = []wantRecord{
	{
		version: Version11,
		fields: []Field{
			{HeaderType, "response"},
			{HeaderRecordID, "<urn:uuid:00000000-0000-4000-8000-000000000001>"},
			{HeaderDate, "2026-09-26T06:00:00Z"},
			{HeaderTargetURI, "http://example.org/ends-in-crlf.html"},
			{HeaderContentType, "application/http; msgtype=response"},
			{HeaderContentLength, "43"},
		},
		// Ends in CRLF. A reader that looked for "\r\n\r\n" instead of
		// counting would stop three octets early, here.
		block: []byte("HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\nend\r\n"),
	},
	{
		version: Version11,
		fields: []Field{
			{HeaderType, "revisit"},
			{HeaderRecordID, "<urn:uuid:00000000-0000-4000-8000-000000000002>"},
			{HeaderDate, "2026-09-26T06:00:00Z"},
			{HeaderTargetURI, "http://example.org/ends-in-crlf.html"},
			{HeaderProfile, "http://netpreserve.org/warc/1.1/revisit/identical-payload-digest"},
			{HeaderContentLength, "0"},
		},
		block: []byte{},
	},
	{
		version: Version11,
		fields: []Field{
			{HeaderType, "resource"},
			{HeaderRecordID, "<urn:uuid:00000000-0000-4000-8000-000000000003>"},
			{HeaderDate, "2026-09-26T06:00:00Z"},
			{HeaderTargetURI, "http://example.org/data/blob.bin?v=2#frag"},
			{HeaderContentType, "application/octet-stream"},
			{HeaderContentLength, "43"},
		},
		block: append(
			[]byte("\x00\x01\x02\rnot-a-crlf\nlf-alone\x00\xff\xfe\xfd"),
			[]byte{0xf0, 0xf1, 0xf2, 0xf3, 0xf4, 0xf5, 0xf6, 0xf7, 0xf8, 0xf9, 0xfa, 0xfb, 0xfc, 0xfd, 0xfe, 0xff}...,
		),
	},
	{
		version: Version11,
		fields: []Field{
			{HeaderType, "metadata"},
			{HeaderRecordID, "<urn:uuid:00000000-0000-4000-8000-000000000004>"},
			{HeaderDate, "2026-09-26T06:00:00Z"},
			{HeaderConcurrentTo, "<urn:uuid:00000000-0000-4000-8000-000000000001>"},
			{HeaderConcurrentTo, "<urn:uuid:00000000-0000-4000-8000-000000000002>"},
			{HeaderConcurrentTo, "<urn:uuid:00000000-0000-4000-8000-000000000003>"},
			{HeaderContentType, "application/warc-fields"},
			{HeaderContentLength, "43"},
		},
		block: []byte("via: http://example.org/\r\nhopsFromSeed: 2\r\n"),
	},
	{
		version: Version11,
		fields: []Field{
			{HeaderType, "metadata"},
			{HeaderRecordID, "<urn:uuid:00000000-0000-4000-8000-000000000005>"},
			{HeaderDate, "2026-09-26T06:00:00Z"},
			{HeaderTargetURI, "https://example.org:8443/a:b/c?q=x:y#z:w"},
			{HeaderPayloadDigest, "sha1:3I42H3S6NNFQ2MSVX7XZKYAYSCX5QBYJ"},
			{"X-Note", "a:b:c: d"},
			{"X-Empty", ""},
			{HeaderContentLength, "6"},
		},
		block: []byte("colons"),
	},
	{
		version: Version11,
		fields: []Field{
			{HeaderType, "metadata"},
			{HeaderRecordID, "<urn:uuid:00000000-0000-4000-8000-000000000006>"},
			{HeaderDate, "2026-09-26T06:00:00Z"},
			{"X-Folded", "first second third"},
			{HeaderContentLength, "6"},
		},
		block: []byte("folded"),
	},
	{
		version: Version10,
		fields: []Field{
			{HeaderType, "future-thing"},
			{HeaderRecordID, "<urn:uuid:00000000-0000-4000-8000-000000000007>"},
			{HeaderDate, "2026-09-26T06:00:00Z"},
			{HeaderTargetURI, "http://example.org/"},
			{HeaderContentLength, "16"},
		},
		block: []byte("a type from 2040"),
	},
}

func checkRecords(t *testing.T, got []*Record, want []wantRecord) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("read %d records, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Version != w.version {
			t.Errorf("record %d: version = %q, want %q", i, got[i].Version, w.version)
		}
		if len(got[i].Header) != len(w.fields) {
			t.Errorf("record %d: %d fields, want %d: got %v", i, len(got[i].Header), len(w.fields), got[i].Header)
		} else {
			for j, f := range w.fields {
				if got[i].Header[j] != f {
					t.Errorf("record %d field %d = %#v, want %#v", i, j, got[i].Header[j], f)
				}
			}
		}
		if !bytes.Equal(got[i].Block, w.block) {
			t.Errorf("record %d block = %q (%d octets), want %q (%d octets)",
				i, got[i].Block, len(got[i].Block), w.block, len(w.block))
		}
	}
}

// TestReadHandbuilt reads the fixture whose octets were laid out from the
// specification, not by this package's Writer.
func TestReadHandbuilt(t *testing.T) {
	checkRecords(t, readAll(t, handbuiltWARC), handbuiltWant)
}

// TestReadWarcio reads the fixture warcio wrote. It is the only test here whose
// expectations we did not choose the framing of.
func TestReadWarcio(t *testing.T) {
	got := readAll(t, warcioWARC)
	want := []wantRecord{
		{
			version: Version11,
			fields: []Field{
				{HeaderType, "warcinfo"},
				{HeaderRecordID, "<urn:uuid:00000000-0000-4000-8000-000000000001>"},
				{HeaderFilename, "warcio.warc"},
				{HeaderDate, "2026-09-26T06:00:00Z"},
				{HeaderBlockDigest, "sha1:O3PD22PB44QSERBHJEISMUZ3QY7PM4E6"},
				{HeaderContentType, "application/warc-fields"},
				{HeaderContentLength, "143"},
			},
			block: []byte("software: warcio (fixture generator for go-filesystems/warc)\r\n" +
				"format: WARC File Format 1.1\r\n" +
				"description: hand-chosen payloads, warcio framing\r\n"),
		},
		{
			version: Version11,
			fields: []Field{
				{HeaderType, "response"},
				{HeaderRecordID, "<urn:uuid:00000000-0000-4000-8000-000000000002>"},
				{HeaderTargetURI, "http://example.org/one.html"},
				{HeaderDate, "2026-09-26T06:00:00Z"},
				{HeaderPayloadDigest, "sha1:CCMSFEBJLF6TLTXELGV3HCAU7SMLAZCI"},
				{HeaderBlockDigest, "sha1:JMYU4G5CHVLAYIER7EJWVXHCTX3PU5LH"},
				{HeaderContentType, "application/http; msgtype=response"},
				{HeaderContentLength, "131"},
			},
			block: []byte("HTTP/1.1 200 OK\r\nContent-Type: text/html; charset=utf-8\r\n" +
				"Content-Length: 52\r\n\r\n<!doctype html>\r\n<title>one</title>\r\n<p>first page\r\n"),
		},
		{
			version: Version11,
			fields: []Field{
				{HeaderType, "request"},
				{HeaderRecordID, "<urn:uuid:00000000-0000-4000-8000-000000000003>"},
				{HeaderTargetURI, "http://example.org/one.html"},
				{HeaderDate, "2026-09-26T06:00:00Z"},
				{HeaderPayloadDigest, "sha1:3I42H3S6NNFQ2MSVX7XZKYAYSCX5QBYJ"},
				{HeaderBlockDigest, "sha1:7OO5COPPLZ2Z4ZVF4WWC2SJYRH2PBQ3Z"},
				{HeaderContentType, "application/http; msgtype=request"},
				{HeaderContentLength, "70"},
			},
			block: []byte("GET /one.html HTTP/1.1\r\nHost: example.org\r\nUser-Agent: fixture/1.0\r\n\r\n"),
		},
		{
			version: Version11,
			fields: []Field{
				{HeaderType, "resource"},
				{HeaderRecordID, "<urn:uuid:00000000-0000-4000-8000-000000000004>"},
				{HeaderTargetURI, "http://example.org/data/blob.bin"},
				{HeaderDate, "2026-09-26T06:00:00Z"},
				{HeaderPayloadDigest, "sha1:GXC7CA2SWH6TBF6NYCSWLPXO6EQ22WJN"},
				{HeaderBlockDigest, "sha1:GXC7CA2SWH6TBF6NYCSWLPXO6EQ22WJN"},
				{HeaderContentType, "application/octet-stream"},
				{HeaderContentLength, "39"},
			},
			block: append(
				[]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15,
					16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31},
				[]byte("\r\n\x00tail")...),
		},
	}
	checkRecords(t, got, want)
}

// TestCorpusReaches asserts the corpus contains the awkward cases, rather than
// trusting that it does. A fixture file can be regenerated, edited or truncated;
// if the case that exercises the framing quietly leaves the corpus, every test
// above still passes and proves less than it did.
func TestCorpusReaches(t *testing.T) {
	recs := append(readAll(t, handbuiltWARC), readAll(t, warcioWARC)...)

	reach := map[string]bool{}
	for _, rec := range recs {
		if bytes.Contains(rec.Block, []byte("\r\n")) {
			reach["block contains CRLF"] = true
		}
		if bytes.HasSuffix(rec.Block, []byte("\r\n")) {
			reach["block ENDS in CRLF"] = true
		}
		if bytes.IndexByte(rec.Block, 0) >= 0 {
			reach["block contains NUL"] = true
		}
		if len(rec.Block) == 0 {
			reach["block is empty"] = true
		}
		names := map[string]int{}
		for _, f := range rec.Header {
			names[strings.ToLower(f.Name)]++
			if strings.Contains(f.Value, ":") {
				reach["value contains a colon"] = true
			}
			if f.Value == "" {
				reach["value is empty"] = true
			}
		}
		for _, n := range names {
			if n > 1 {
				reach["field name repeats"] = true
			}
		}
		if rec.Version == Version10 {
			reach["version is WARC/1.0"] = true
		}
		switch rec.Type() {
		case TypeWarcinfo, TypeResponse, TypeRequest, TypeMetadata,
			TypeResource, TypeRevisit, TypeConversion, TypeContinuation:
		default:
			reach["WARC-Type is unregistered"] = true
		}
	}
	// A folded value is not visible in the parsed record -- unfolding is the
	// point -- so it is asserted against the fixture's octets.
	if bytes.Contains(handbuiltWARC, []byte("\r\n\tsecond\r\n")) {
		reach["header value is folded"] = true
	}

	for _, want := range []string{
		"block contains CRLF", "block ENDS in CRLF", "block contains NUL",
		"block is empty", "value contains a colon", "value is empty",
		"field name repeats", "version is WARC/1.0", "WARC-Type is unregistered",
		"header value is folded",
	} {
		if !reach[want] {
			t.Errorf("corpus does not reach: %s", want)
		}
	}
}

// TestFoldedValueEqualsUnfolded is the ablation guard for unfolding: the folded
// record and the same fields on one line must parse to the same value, so a
// reader that dropped folding support, or that joined the lines without the
// single SP the grammar means, fails here.
func TestFoldedValueEqualsUnfolded(t *testing.T) {
	const flat = "WARC/1.1\r\n" +
		"WARC-Type: metadata\r\n" +
		"WARC-Record-ID: <urn:uuid:00000000-0000-4000-8000-000000000006>\r\n" +
		"WARC-Date: 2026-09-26T06:00:00Z\r\n" +
		"X-Folded: first second third\r\n" +
		"Content-Length: 6\r\n" +
		"\r\nfolded\r\n\r\n"
	got := readAll(t, []byte(flat))
	checkRecords(t, got, handbuiltWant[5:6])
}

// validRecord is a syntactically complete record used as the base for the
// negative cases: each one mutates exactly one thing about it.
const validRecord = "WARC/1.1\r\n" +
	"WARC-Type: metadata\r\n" +
	"WARC-Record-ID: <urn:uuid:1>\r\n" +
	"WARC-Date: 2026-09-26T06:00:00Z\r\n" +
	"Content-Length: 2\r\n" +
	"\r\nhi\r\n\r\n"

func TestReadRejects(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty version line", "\r\n", "unsupported version line"},
		{"not a warc at all", "PK\x03\x04\r\n", "unsupported version line"},
		{"version 2.0", "WARC/2.0\r\nContent-Length: 0\r\n\r\n\r\n\r\n", "unsupported version line"},
		{"lowercase version", "warc/1.1\r\nContent-Length: 0\r\n\r\n\r\n\r\n", "unsupported version line"},
		{"LF-terminated version line", "WARC/1.1\n", "LF-terminated, want CRLF"},
		{"version line not terminated", "WARC/1.1", "not CRLF-terminated at end of input"},
		{"CR inside a line", "WARC/1.1\r\nWARC-Type: a\rb\r\n\r\n", "CR that does not end it"},
		{"header without a colon", "WARC/1.1\r\nWARC-Type\r\n\r\n", "has no colon"},
		{"field name with a space", "WARC/1.1\r\nWARC Type: metadata\r\n\r\n", "invalid field name"},
		{"field name empty", "WARC/1.1\r\n: value\r\n\r\n", "invalid field name"},
		{"continuation before any field", "WARC/1.1\r\n  orphan\r\n\r\n", "starts with a continuation line"},
		{"header block never ends", "WARC/1.1\r\nWARC-Type: metadata\r\n", "header: unexpected EOF"},
		{"no Content-Length", "WARC/1.1\r\nWARC-Type: metadata\r\n\r\n", "has 0 Content-Length fields"},
		{"two Content-Lengths",
			"WARC/1.1\r\nWARC-Type: metadata\r\nContent-Length: 2\r\nContent-Length: 3\r\n\r\nhi\r\n\r\n",
			"has 2 Content-Length fields"},
		{"Content-Length not a number",
			"WARC/1.1\r\nWARC-Type: metadata\r\nContent-Length: two\r\n\r\nhi\r\n\r\n",
			"not a decimal octet count"},
		{"Content-Length signed",
			"WARC/1.1\r\nWARC-Type: metadata\r\nContent-Length: +2\r\n\r\nhi\r\n\r\n",
			"not a decimal octet count"},
		{"Content-Length overflows int64",
			"WARC/1.1\r\nWARC-Type: metadata\r\nContent-Length: 99999999999999999999\r\n\r\nhi\r\n\r\n",
			"value out of range"},
		{"missing WARC-Type",
			"WARC/1.1\r\nWARC-Record-ID: <urn:uuid:1>\r\nWARC-Date: 2026-09-26T06:00:00Z\r\nContent-Length: 0\r\n\r\n\r\n\r\n",
			"missing mandatory field WARC-Type"},
		{"missing WARC-Date",
			"WARC/1.1\r\nWARC-Type: metadata\r\nWARC-Record-ID: <urn:uuid:1>\r\nContent-Length: 0\r\n\r\n\r\n\r\n",
			"missing mandatory field WARC-Date"},
		{"missing WARC-Record-ID",
			"WARC/1.1\r\nWARC-Type: metadata\r\nWARC-Date: 2026-09-26T06:00:00Z\r\nContent-Length: 0\r\n\r\n\r\n\r\n",
			"missing mandatory field WARC-Record-ID"},
		{"record ID without angle brackets",
			strings.Replace(validRecord, "<urn:uuid:1>", "urn:uuid:1", 1),
			"not a URI in angle brackets"},
		{"record ID too short",
			strings.Replace(validRecord, "<urn:uuid:1>", "<>", 1),
			"not a URI in angle brackets"},
		{"date with a numeric offset",
			strings.Replace(validRecord, "2026-09-26T06:00:00Z", "2026-09-26T06:00:00+00:00", 1),
			"not a UTC W3C-DTF timestamp"},
		{"date not a timestamp",
			strings.Replace(validRecord, "2026-09-26T06:00:00Z", "yesterdayZ", 1),
			`WARC-Date "yesterdayZ"`},
		{"block shorter than Content-Length",
			"WARC/1.1\r\nWARC-Type: metadata\r\nWARC-Record-ID: <urn:uuid:1>\r\n" +
				"WARC-Date: 2026-09-26T06:00:00Z\r\nContent-Length: 99\r\n\r\nhi\r\n\r\n",
			"unexpected EOF"},
		{"trailer is only one CRLF",
			strings.TrimSuffix(validRecord, "\r\n"),
			"unexpected EOF"},
		{"trailer is LF LF",
			strings.Replace(validRecord, "hi\r\n\r\n", "hi\n\n\n\n", 1),
			`record trailer is "\n\n\n\n"`},
		{"trailer eaten by a longer block",
			// Content-Length 2 but the block is four octets: the two
			// trailing CRLFs then land on the wrong bytes.
			strings.Replace(validRecord, "\r\nhi\r\n\r\n", "\r\nhi!!\r\n\r\n", 1),
			`record trailer is "!!\r\n"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewReader(strings.NewReader(tc.in))
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			_, err = r.Next()
			if err == nil {
				t.Fatalf("Next succeeded, want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Next error = %v, want it to contain %q", err, tc.want)
			}
			// Every complaint above is about syntax or a truncation, never
			// an I/O failure, so exactly one of the two classifications
			// must hold -- a caller has to be able to tell "this is not a
			// WARC" from "the disk went away".
			syntax := errors.Is(err, ErrSyntax)
			truncated := errors.Is(err, io.ErrUnexpectedEOF)
			if !syntax && !truncated {
				t.Errorf("error %v is neither ErrSyntax nor io.ErrUnexpectedEOF", err)
			}
			// Errors are sticky.
			if _, again := r.Next(); again == nil || again.Error() != err.Error() {
				t.Errorf("second Next = %v, want the same error %v", again, err)
			}
		})
	}
}

// TestReadStopsAtCleanEOF: io.EOF only where a record has just ended.
func TestReadStopsAtCleanEOF(t *testing.T) {
	r, err := NewReader(strings.NewReader(validRecord))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if _, err := r.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}
	for i := range 2 {
		if _, err := r.Next(); !errors.Is(err, io.EOF) {
			t.Fatalf("Next %d after last record = %v, want io.EOF", i, err)
		}
	}
	if _, err := NewReader(strings.NewReader("")); err != nil {
		t.Fatalf("NewReader(empty): %v", err)
	}
	empty, _ := NewReader(strings.NewReader(""))
	if _, err := empty.Next(); !errors.Is(err, io.EOF) {
		t.Errorf("Next on an empty stream = %v, want io.EOF", err)
	}
}

// TestNewReaderNil: a nil reader is caught at the call that was wrong rather
// than panicking inside the first Next.
func TestNewReaderNil(t *testing.T) {
	if _, err := NewReader(nil); err == nil {
		t.Fatal("NewReader(nil) succeeded")
	}
}

// TestReadIOErrors: a failure from the underlying reader reaches the caller as
// itself, unwrappable, at each of the three places one can arrive.
func TestReadIOErrors(t *testing.T) {
	boom := errors.New("disk on fire")
	for _, tc := range []struct {
		name string
		n    int
	}{
		{"during the version line", 4},
		{"during the header block", 20},
		{"during the block", len(validRecord) - 4},
		{"during the trailer", len(validRecord) - 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewReader(&errReader{data: []byte(validRecord), n: tc.n, err: boom})
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			_, err = r.Next()
			if !errors.Is(err, boom) {
				t.Fatalf("Next = %v, want it to wrap %v", err, boom)
			}
			if errors.Is(err, ErrSyntax) {
				t.Errorf("an I/O failure was reported as ErrSyntax: %v", err)
			}
			if !strings.HasPrefix(err.Error(), "warc: record 0:") {
				t.Errorf("error %q does not say which record failed", err)
			}
		})
	}
}

// TestBlockIsNotAllocatedFromContentLength: a header claiming an enormous block
// must not make the reader allocate it. The record here is eighty octets long
// and says its block is eight exabytes; a reader doing make([]byte, clen) dies
// with an out-of-memory panic instead of returning an error.
func TestBlockIsNotAllocatedFromContentLength(t *testing.T) {
	in := "WARC/1.1\r\nWARC-Type: metadata\r\nWARC-Record-ID: <urn:uuid:1>\r\n" +
		"WARC-Date: 2026-09-26T06:00:00Z\r\nContent-Length: 9000000000000000000\r\n\r\nhi\r\n\r\n"
	r, err := NewReader(strings.NewReader(in))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if _, err := r.Next(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Next = %v, want io.ErrUnexpectedEOF", err)
	}
}

// TestReadAllOfManyRecords frames a longer file, so an off-by-one in the
// trailer would show up as a misread record rather than as a lucky pass on a
// two-record fixture.
func TestReadAllOfManyRecords(t *testing.T) {
	var in bytes.Buffer
	var want []wantRecord
	for i := range 40 {
		// Blocks of every length from 0 to 39, each ending in CRLF once it
		// is long enough to: the trailer must be found by count.
		block := bytes.Repeat([]byte("x"), i)
		if i >= 2 {
			block = append(block[:i-2], '\r', '\n')
		}
		fmt.Fprintf(&in, "WARC/1.1\r\nWARC-Type: resource\r\nWARC-Record-ID: <urn:uuid:%d>\r\n"+
			"WARC-Date: 2026-09-26T06:00:00Z\r\nContent-Length: %d\r\n\r\n", i, len(block))
		in.Write(block)
		in.WriteString("\r\n\r\n")
		want = append(want, wantRecord{
			version: Version11,
			fields: []Field{
				{HeaderType, "resource"},
				{HeaderRecordID, fmt.Sprintf("<urn:uuid:%d>", i)},
				{HeaderDate, "2026-09-26T06:00:00Z"},
				{HeaderContentLength, fmt.Sprint(len(block))},
			},
			block: block,
		})
	}
	checkRecords(t, readAll(t, in.Bytes()), want)
}

// TestHeaderValueTrimsOnlySPandHT pins WHICH octets a reader may strip from a
// field value, and exists because an ablation found the corpus blind to it.
//
// ISO 28500 §4 takes LWS from RFC 2616, and LWS is SP and HT -- nothing else. A
// reader that reaches for strings.TrimSpace instead also strips VT, FF and every
// Unicode space, U+00A0 among them; those are ordinary field-content, so the
// value it hands back is missing octets the file carried, with nothing to say so.
// Replacing strings.Trim(value, " \t") with strings.TrimSpace(value) passed the
// whole suite before this test existed.
func TestHeaderValueTrimsOnlySPandHT(t *testing.T) {
	const value = "\xc2\xa0\x0b\x0cvalue\x0c\x0b\xc2\xa0"
	in := "WARC/1.1\r\n" +
		"WARC-Type: metadata\r\n" +
		"WARC-Record-ID: <urn:uuid:1>\r\n" +
		"WARC-Date: 2026-09-26T06:00:00Z\r\n" +
		// One SP after the colon and one HT before the CRLF: both are LWS
		// and both must go, while what they surround must not.
		"X-Edges: \t" + value + " \t\r\n" +
		"Content-Length: 2\r\n\r\nhi\r\n\r\n"
	got := readAll(t, []byte(in))
	if len(got) != 1 {
		t.Fatalf("read %d records, want 1", len(got))
	}
	if v := got[0].Header.Get("X-Edges"); v != value {
		t.Errorf("X-Edges = %q, want %q", v, value)
	}
	// And the value survives a round trip through the Writer, which must not
	// refuse it: it has no leading or trailing SP or HT.
	var buf bytes.Buffer
	if err := NewWriter(&buf).WriteRecord(got[0]); err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}
	if back := readAll(t, buf.Bytes()); back[0].Header.Get("X-Edges") != value {
		t.Errorf("after a round trip X-Edges = %q, want %q", back[0].Header.Get("X-Edges"), value)
	}
}
