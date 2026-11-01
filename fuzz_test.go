// SPDX-License-Identifier: BSD-3-Clause

package warc

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func fuzzSeeds(f *testing.F) {
	f.Add(handbuiltWARC)
	f.Add(warcioWARC)
	f.Add([]byte(validRecord))
	f.Add([]byte(validRecord + validRecord))
	f.Add([]byte(bombHead + "X: a\r\n b\r\n c\r\nContent-Length: 0\r\n\r\n\r\n\r\n"))
	f.Add([]byte(bombHead + "Content-Length: 9223372036854775807\r\n\r\nhi\r\n\r\n"))
}

// FuzzReader feeds Next arbitrary bytes. Invariants: no panic, no unbounded
// allocation (the header budget and the stream's own length bound everything),
// the position it reports matches what it consumed, and errors stay short.
func FuzzReader(f *testing.F) {
	fuzzSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := NewReader(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		for {
			rec, err := r.Next()
			if err != nil {
				if len(err.Error()) > 4096 {
					t.Fatalf("error of %d bytes", len(err.Error()))
				}
				return
			}
			if r.pos > int64(len(b)) {
				t.Fatalf("pos %d beyond the %d-byte input", r.pos, len(b))
			}
			if int64(len(rec.Block)) != r.blockLen {
				t.Fatalf("block %d bytes, blockLen %d", len(rec.Block), r.blockLen)
			}
		}
	})
}

// FuzzOpen is differential: Open steps over blocks and Next reads them, and on
// any input both accept they must frame it identically, record for record.
func FuzzOpen(f *testing.F) {
	fuzzSeeds(f)
	f.Fuzz(func(t *testing.T, b []byte) {
		fsys, oerr := Open(bytes.NewReader(b), int64(len(b)))
		r, _ := NewReader(bytes.NewReader(b))
		var recs []*Record
		var rerr error
		for {
			var rec *Record
			rec, rerr = r.Next()
			if rerr != nil {
				break
			}
			recs = append(recs, rec)
		}
		readerOK := errors.Is(rerr, io.EOF)
		if (oerr == nil) != readerOK {
			t.Fatalf("Open: %v; Next ended with %v", oerr, rerr)
		}
		if oerr != nil {
			return
		}
		names := fsys.Names()
		if len(names) != len(recs) {
			t.Fatalf("Open found %d records, Next %d", len(names), len(recs))
		}
		for i, name := range names {
			got, err := fsys.ReadFile(name)
			if err != nil || !bytes.Equal(got, recs[i].Block) {
				t.Fatalf("%s: ReadFile (%d bytes, %v) differs from Next (%d bytes)",
					name, len(got), err, len(recs[i].Block))
			}
		}
	})
}
