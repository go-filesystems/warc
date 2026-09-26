// SPDX-License-Identifier: BSD-3-Clause

package warc

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// TestWriteBytes pins the octets. The expected string here is written out from
// ISO 28500 §5's grammar -- version line, "name: value" per field, CRLF, the
// block, CRLF, CRLF -- and not from running the Writer and copying what came
// out, which would pin whatever it does rather than what the format says.
func TestWriteBytes(t *testing.T) {
	cases := []struct {
		name string
		rec  *Record
		want string
	}{
		{
			name: "block ending in CRLF, Content-Length supplied",
			rec: &Record{
				Version: Version11,
				Header: Header{
					{HeaderType, "response"},
					{HeaderRecordID, "<urn:uuid:1>"},
					{HeaderDate, "2026-09-26T06:00:00Z"},
					{HeaderContentLength, "6"},
				},
				Block: []byte("ab\r\ncd"),
			},
			want: "WARC/1.1\r\nWARC-Type: response\r\nWARC-Record-ID: <urn:uuid:1>\r\n" +
				"WARC-Date: 2026-09-26T06:00:00Z\r\nContent-Length: 6\r\n\r\nab\r\ncd\r\n\r\n",
		},
		{
			name: "no version means 1.1, no Content-Length means computed",
			rec: &Record{
				Header: Header{
					{HeaderType, "metadata"},
					{HeaderRecordID, "<urn:uuid:2>"},
					{HeaderDate, "2026-09-26T06:00:00Z"},
				},
				Block: []byte("four"),
			},
			want: "WARC/1.1\r\nWARC-Type: metadata\r\nWARC-Record-ID: <urn:uuid:2>\r\n" +
				"WARC-Date: 2026-09-26T06:00:00Z\r\nContent-Length: 4\r\n\r\nfour\r\n\r\n",
		},
		{
			// A zero-length block is six octets of framing and no payload.
			// A writer that skipped the trailer for an empty block would
			// produce a file whose NEXT record cannot be found.
			name: "zero-length block still carries both CRLFs",
			rec: &Record{
				Version: Version10,
				Header: Header{
					{HeaderType, "revisit"},
					{HeaderRecordID, "<urn:uuid:3>"},
					{HeaderDate, "2026-09-26T06:00:00Z"},
				},
			},
			want: "WARC/1.0\r\nWARC-Type: revisit\r\nWARC-Record-ID: <urn:uuid:3>\r\n" +
				"WARC-Date: 2026-09-26T06:00:00Z\r\nContent-Length: 0\r\n\r\n\r\n\r\n",
		},
		{
			name: "NUL in the block, repeated field, colons in a value",
			rec: &Record{
				Header: Header{
					{HeaderType, "resource"},
					{HeaderRecordID, "<urn:uuid:4>"},
					{HeaderDate, "2026-09-26T06:00:00.500Z"},
					{HeaderConcurrentTo, "<urn:uuid:1>"},
					{HeaderConcurrentTo, "<urn:uuid:2>"},
					{HeaderTargetURI, "https://example.org:8443/a:b?c=d:e"},
					{"X-Empty", ""},
				},
				Block: []byte{0x00, 0xff, '\r', '\n', 0x00},
			},
			want: "WARC/1.1\r\nWARC-Type: resource\r\nWARC-Record-ID: <urn:uuid:4>\r\n" +
				"WARC-Date: 2026-09-26T06:00:00.500Z\r\n" +
				"WARC-Concurrent-To: <urn:uuid:1>\r\nWARC-Concurrent-To: <urn:uuid:2>\r\n" +
				"WARC-Target-URI: https://example.org:8443/a:b?c=d:e\r\n" +
				"X-Empty: \r\nContent-Length: 5\r\n\r\n\x00\xff\r\n\x00\r\n\r\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := NewWriter(&buf)
			if err := w.WriteRecord(tc.rec); err != nil {
				t.Fatalf("WriteRecord: %v", err)
			}
			if got := buf.String(); got != tc.want {
				t.Errorf("wrote %q\n want %q", got, tc.want)
			}
			if w.Records() != 1 {
				t.Errorf("Records() = %d, want 1", w.Records())
			}
		})
	}
}

// TestWriterReproducesWarcio re-emits the records read from the warcio fixture
// and compares the result to the fixture's octets.
//
// This is the check that keeps the two halves of this package from agreeing with
// each other about a mistake: warcio chose the framing, and byte equality here
// means our Writer put every CRLF where an implementation we did not write put
// it. A Writer↔Reader round trip alone cannot say that.
func TestWriterReproducesWarcio(t *testing.T) {
	recs := readAll(t, warcioWARC)
	var buf bytes.Buffer
	w := NewWriter(&buf)
	for i, rec := range recs {
		if err := w.WriteRecord(rec); err != nil {
			t.Fatalf("WriteRecord %d: %v", i, err)
		}
	}
	if !bytes.Equal(buf.Bytes(), warcioWARC) {
		t.Errorf("re-emitted file differs from the warcio fixture\n got %d octets\nwant %d octets",
			buf.Len(), len(warcioWARC))
		for i := 0; i < min(buf.Len(), len(warcioWARC)); i++ {
			if buf.Bytes()[i] != warcioWARC[i] {
				t.Errorf("first difference at octet %d: got %q, want %q", i,
					buf.Bytes()[max(0, i-20):min(buf.Len(), i+20)],
					warcioWARC[max(0, i-20):min(len(warcioWARC), i+20)])
				break
			}
		}
	}
}

// TestRoundTripBothDirections: every record of the hand-built fixture survives
// Writer then Reader unchanged. The one record that is not byte-identical is the
// folded one, by design -- unfolding is what the grammar's LWS means -- so the
// assertion is on the parsed records, which must be equal.
func TestRoundTripBothDirections(t *testing.T) {
	recs := readAll(t, handbuiltWARC)
	var buf bytes.Buffer
	w := NewWriter(&buf)
	for i, rec := range recs {
		if err := w.WriteRecord(rec); err != nil {
			t.Fatalf("WriteRecord %d: %v", i, err)
		}
	}
	checkRecords(t, readAll(t, buf.Bytes()), handbuiltWant)

	// And a second round trip is a fixed point: if the first one lost or
	// added an octet the second would move again.
	var buf2 bytes.Buffer
	w2 := NewWriter(&buf2)
	for _, rec := range readAll(t, buf.Bytes()) {
		if err := w2.WriteRecord(rec); err != nil {
			t.Fatalf("WriteRecord: %v", err)
		}
	}
	if !bytes.Equal(buf.Bytes(), buf2.Bytes()) {
		t.Error("write(read(write(read(f)))) differs from write(read(f)): the round trip is not a fixed point")
	}
}

func TestWriteRefuses(t *testing.T) {
	good := func() Header {
		return Header{
			{HeaderType, "metadata"},
			{HeaderRecordID, "<urn:uuid:1>"},
			{HeaderDate, "2026-09-26T06:00:00Z"},
		}
	}
	cases := []struct {
		name string
		rec  *Record
		want string
	}{
		{"unsupported version", &Record{Version: "WARC/0.9", Header: good()}, "unsupported version"},
		{"not a WARC version at all", &Record{Version: "HTTP/1.1", Header: good()}, "unsupported version"},
		{"missing WARC-Type", &Record{Header: Header{
			{HeaderRecordID, "<urn:uuid:1>"}, {HeaderDate, "2026-09-26T06:00:00Z"}}},
			"missing mandatory field WARC-Type"},
		{"missing WARC-Date", &Record{Header: Header{
			{HeaderType, "metadata"}, {HeaderRecordID, "<urn:uuid:1>"}}},
			"missing mandatory field WARC-Date"},
		{"missing WARC-Record-ID", &Record{Header: Header{
			{HeaderType, "metadata"}, {HeaderDate, "2026-09-26T06:00:00Z"}}},
			"missing mandatory field WARC-Record-ID"},
		{"date is not UTC", &Record{Header: Header{
			{HeaderType, "metadata"}, {HeaderRecordID, "<urn:uuid:1>"},
			{HeaderDate, "2026-09-26T06:00:00+02:00"}}},
			"not a UTC W3C-DTF timestamp"},
		{"record ID without brackets", &Record{Header: Header{
			{HeaderType, "metadata"}, {HeaderRecordID, "urn:uuid:1"},
			{HeaderDate, "2026-09-26T06:00:00Z"}}},
			"not a URI in angle brackets"},
		{"field name with a space", &Record{Header: append(good(), Field{"X Note", "v"})},
			`"X Note" is not a valid field name`},
		{"field name with a colon", &Record{Header: append(good(), Field{"X:Note", "v"})},
			"is not a valid field name"},
		{"value containing CRLF", &Record{Header: append(good(), Field{"X-Note", "a\r\nWARC-Type: injected"})},
			"contains"},
		{"value containing LF", &Record{Header: append(good(), Field{"X-Note", "a\nb"})}, "contains"},
		{"value containing NUL", &Record{Header: append(good(), Field{"X-Note", "a\x00b"})}, "contains"},
		{"value with a leading space", &Record{Header: append(good(), Field{"X-Note", " a"})},
			"leading or trailing whitespace"},
		{"value with a trailing tab", &Record{Header: append(good(), Field{"X-Note", "a\t"})},
			"leading or trailing whitespace"},
		{"two Content-Length fields", &Record{Header: append(good(),
			Field{HeaderContentLength, "0"}, Field{HeaderContentLength, "0"})},
			"two Content-Length fields"},
		{"Content-Length not a number", &Record{Header: append(good(), Field{HeaderContentLength, "nine"})},
			"not a decimal octet count"},
		{"Content-Length disagrees with the block", &Record{
			Header: append(good(), Field{HeaderContentLength, "9"}), Block: []byte("two")},
			"Content-Length says 9, block is 3 octets"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := NewWriter(&buf)
			err := w.WriteRecord(tc.rec)
			if err == nil {
				t.Fatalf("WriteRecord succeeded, want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.want)
			}
			if !errors.Is(err, ErrSyntax) {
				t.Errorf("error %v does not satisfy errors.Is(err, ErrSyntax)", err)
			}
			// A refused record must not have written anything: a
			// half-written record is the one thing worse than a rejection.
			if buf.Len() != 0 {
				t.Errorf("a refused record wrote %d octets: %q", buf.Len(), buf.String())
			}
			// And the error is sticky.
			if again := w.WriteRecord(&Record{Header: good()}); again == nil {
				t.Error("WriteRecord after a failure succeeded; errors must be sticky")
			}
		})
	}
}

// TestWriteIOErrors: each of the three writes a record costs can fail, and each
// failure reaches the caller unwrapped.
func TestWriteIOErrors(t *testing.T) {
	boom := errors.New("pipe closed")
	for _, n := range []int{1, 2, 3} {
		w := NewWriter(&failWriter{n: n, err: boom})
		err := w.WriteRecord(&Record{
			Header: Header{
				{HeaderType, "metadata"},
				{HeaderRecordID, "<urn:uuid:1>"},
				{HeaderDate, "2026-09-26T06:00:00Z"},
			},
			Block: []byte("body"),
		})
		if !errors.Is(err, boom) {
			t.Errorf("write %d: WriteRecord = %v, want it to wrap %v", n, err, boom)
		}
		if w.Records() != 0 {
			t.Errorf("write %d: Records() = %d after a failure, want 0", n, w.Records())
		}
	}
}
