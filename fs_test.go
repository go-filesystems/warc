// SPDX-License-Identifier: BSD-3-Clause

package warc

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"testing"

	filesystem "github.com/go-filesystems/interface"
)

func openHandbuilt(t *testing.T) *FS {
	t.Helper()
	f, err := Open(bytes.NewReader(handbuiltWARC), int64(len(handbuiltWARC)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return f
}

// TestEntryNames pins the naming scheme. The names are written out here rather
// than derived, because the point of the scheme is that a person can predict it
// from the archive: a test that computed the expectation the same way the code
// does would agree with any scheme at all.
func TestEntryNames(t *testing.T) {
	want := []string{
		"00000-response-ends-in-crlf.html",
		"00001-revisit-ends-in-crlf.html",
		"00002-resource-blob.bin",
		"00003-metadata",
		"00004-metadata-c",
		"00005-metadata",
		"00006-future-thing-example.org",
	}
	got := openHandbuilt(t).Names()
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %q, want %q", i, got[i], want[i])
		}
	}

	// Two records with the same type and the same target URI -- a request and
	// a response for one page, which every crawl produces -- must NOT collide.
	// This is the case a name built from the target URI, or from the type and
	// the URI, loses silently.
	names := openHandbuilt(t).Names()
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Errorf("duplicate entry name %q", n)
		}
		seen[n] = true
	}
	warcioNames, err := Open(bytes.NewReader(warcioWARC), int64(len(warcioWARC)))
	if err != nil {
		t.Fatalf("Open(warcio): %v", err)
	}
	wn := warcioNames.Names()
	if wn[1] == wn[2] {
		t.Fatalf("request and response for one URI got the same name %q", wn[1])
	}
	if wn[1] != "00001-response-one.html" || wn[2] != "00002-request-one.html" {
		t.Errorf("warcio names[1:3] = %q, want the ordinal-type-tail form", wn[1:3])
	}
}

func TestEntryName(t *testing.T) {
	for _, tc := range []struct {
		ordinal   int
		warcType  string
		targetURI string
		want      string
	}{
		{0, "warcinfo", "", "00000-warcinfo"},
		{7, "RESPONSE", "http://e.org/a.html", "00007-response-a.html"},
		{12345, "response", "http://e.org/a.html", "12345-response-a.html"},
		// Five digits is a floor, not a cap: a WARC with more than 100000
		// records keeps sorting correctly because the field widens.
		{1234567, "response", "http://e.org/a.html", "1234567-response-a.html"},
		// The query and the fragment are not part of the name.
		{1, "resource", "http://e.org/dir/f.png?v=2#x", "00001-resource-f.png"},
		// A trailing slash leaves no segment, so the authority is the tail.
		{2, "response", "http://example.org/", "00002-response-example.org"},
		{3, "response", "http://example.org", "00003-response-example.org"},
		{4, "response", "http://example.org/a/b/c/", "00004-response-c"},
		// A scheme with no authority, and one with no path at all.
		{5, "resource", "urn:isbn:0451450523", "00005-resource-urn_isbn_0451450523"},
		{6, "resource", "dns:example.org", "00006-resource-dns_example.org"},
		// Anything outside [A-Za-z0-9._-] folds to a single underscore, and
		// the result is trimmed of leading and trailing punctuation, so the
		// name cannot start with a dot or end with a separator.
		// Percent escapes are NOT decoded: %20 stays three characters, so
		// "%20c" folds to "_20c" and the leading separator is trimmed. A name
		// that decoded them could produce a '/' or a NUL from an escape.
		{8, "response", "http://e.org/a  b//..//%20c%%%d.txt", "00008-response-20c_d.txt"},
		{9, "response", "http://e.org/" + strings.Repeat("x", 80),
			"00009-response-" + strings.Repeat("x", 48)},
		// A URI whose tail sanitises to nothing is left off entirely rather
		// than leaving a dangling separator.
		{10, "response", "http://///", "00010-response"},
		{11, "response", "http://e.org/%%%", "00011-response"},
		// A type that sanitises to nothing still names something.
		{12, "???", "", "00012-record"},
		{13, "", "", "00013-record"},
	} {
		if got := EntryName(tc.ordinal, tc.warcType, tc.targetURI); got != tc.want {
			t.Errorf("EntryName(%d, %q, %q) = %q, want %q", tc.ordinal, tc.warcType, tc.targetURI, got, tc.want)
		}
	}
}

// TestReadFileIsTheBlock: an entry's contents are the record's content block,
// octet for octet, including the awkward ones.
func TestReadFileIsTheBlock(t *testing.T) {
	f := openHandbuilt(t)
	names := f.Names()
	for i, want := range handbuiltWant {
		got, err := f.ReadFile(names[i])
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", names[i], err)
		}
		if !bytes.Equal(got, want.block) {
			t.Errorf("ReadFile(%q) = %q, want %q", names[i], got, want.block)
		}
		// Reachable by every spelling of the path.
		for _, p := range []string{names[i], "/" + names[i], "./" + names[i], "/a/../" + names[i]} {
			again, err := f.ReadFile(p)
			if err != nil || !bytes.Equal(again, want.block) {
				t.Errorf("ReadFile(%q) = %q, %v; want the same block", p, again, err)
			}
		}
	}
}

// TestHeaderAndRecord: the named fields are not lost by the filesystem view.
func TestHeaderAndRecord(t *testing.T) {
	f := openHandbuilt(t)
	names := f.Names()

	hdr, err := f.Header(names[3])
	if err != nil {
		t.Fatalf("Header: %v", err)
	}
	if got := hdr.Values(HeaderConcurrentTo); len(got) != 3 {
		t.Errorf("Header(%q).Values(WARC-Concurrent-To) = %v, want 3 values", names[3], got)
	}
	// The copy must not alias the FS's own header: mutating it here must not
	// change what the next call returns.
	hdr.Del(HeaderConcurrentTo)
	if again, _ := f.Header(names[3]); len(again.Values(HeaderConcurrentTo)) != 3 {
		t.Error("Header returned an alias of the FS's own header, not a copy")
	}

	rec, err := f.Record(names[6])
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if rec.Version != Version10 || rec.Type() != "future-thing" {
		t.Errorf("Record(%q) = %q %q, want WARC/1.0 future-thing", names[6], rec.Version, rec.Type())
	}
	if !bytes.Equal(rec.Block, []byte("a type from 2040")) {
		t.Errorf("Record(%q).Block = %q", names[6], rec.Block)
	}
	if _, err := f.Header("nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Header(missing) = %v, want fs.ErrNotExist", err)
	}
	if _, err := f.Record("nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Record(missing) = %v, want fs.ErrNotExist", err)
	}
}

func TestListDirAndStat(t *testing.T) {
	f := openHandbuilt(t)
	names := f.Names()

	for _, root := range []string{"/", "", ".", "//", "/."} {
		ents, err := f.ListDir(root)
		if err != nil {
			t.Fatalf("ListDir(%q): %v", root, err)
		}
		if len(ents) != len(names) {
			t.Fatalf("ListDir(%q) = %d entries, want %d", root, len(ents), len(names))
		}
		inodes := map[uint64]bool{}
		for i, e := range ents {
			if e.Name() != names[i] {
				t.Errorf("ListDir(%q)[%d].Name() = %q, want %q", root, i, e.Name(), names[i])
			}
			if e.FileType() != dtReg {
				t.Errorf("%q FileType = %d, want DT_REG (%d)", e.Name(), e.FileType(), dtReg)
			}
			if e.Inode() == rootInode {
				t.Errorf("%q has the root's inode", e.Name())
			}
			if inodes[e.Inode()] {
				t.Errorf("%q reuses inode %d", e.Name(), e.Inode())
			}
			inodes[e.Inode()] = true
		}
	}

	st, err := f.Stat("/")
	if err != nil {
		t.Fatalf("Stat(/): %v", err)
	}
	if st.Mode() != modeDir || st.Inode() != rootInode || st.Size() != 0 {
		t.Errorf("Stat(/) = mode %o inode %d size %d, want %o %d 0", st.Mode(), st.Inode(), st.Size(), modeDir, rootInode)
	}
	for i, name := range names {
		st, err := f.Stat(name)
		if err != nil {
			t.Fatalf("Stat(%q): %v", name, err)
		}
		if st.Mode() != modeReg {
			t.Errorf("Stat(%q).Mode() = %o, want %o", name, st.Mode(), modeReg)
		}
		// Stat's size is the number ReadFile returns, which is the record's
		// Content-Length.
		if st.Size() != uint64(len(handbuiltWant[i].block)) {
			t.Errorf("Stat(%q).Size() = %d, want %d", name, st.Size(), len(handbuiltWant[i].block))
		}
	}

	// A file is not a directory, and a name that is not there is reported as
	// not there -- the error contract go-filesystems/interface states.
	if _, err := f.ListDir(names[0]); !errors.Is(err, fs.ErrInvalid) {
		t.Errorf("ListDir(a file) = %v, want fs.ErrInvalid", err)
	}
	if _, err := f.ListDir("/nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ListDir(missing) = %v, want fs.ErrNotExist", err)
	}
	if _, err := f.Stat("/nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat(missing) = %v, want fs.ErrNotExist", err)
	}
	if _, err := f.ReadFile("/nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadFile(missing) = %v, want fs.ErrNotExist", err)
	}
	if _, err := f.ReadFile("/"); !errors.Is(err, fs.ErrInvalid) {
		t.Errorf("ReadFile(/) = %v, want fs.ErrInvalid", err)
	}
}

func TestReadLink(t *testing.T) {
	f := openHandbuilt(t)
	for _, p := range []string{"/", f.Names()[0]} {
		if _, err := f.ReadLink(p); !errors.Is(err, fs.ErrInvalid) {
			t.Errorf("ReadLink(%q) = %v, want fs.ErrInvalid", p, err)
		}
	}
	if _, err := f.ReadLink("/nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadLink(missing) = %v, want fs.ErrNotExist", err)
	}
}

// TestOpener exercises the optional capability this driver implements, and
// checks the parts of io.ReaderAt's contract a caller relies on.
func TestOpener(t *testing.T) {
	var ifs filesystem.Filesystem
	ifs, err := OpenReader(bytes.NewReader(handbuiltWARC), int64(len(handbuiltWARC)))
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	o, ok := ifs.(filesystem.Opener)
	if !ok {
		t.Fatal("the filesystem does not implement filesystem.Opener")
	}
	names := ifs.(*FS).Names()

	// Record 2's block is the binary one: NUL, a stray CR, high bytes.
	want := handbuiltWant[2].block
	file, err := o.OpenFile(names[2])
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	if file.Size() != int64(len(want)) {
		t.Fatalf("Size() = %d, want %d", file.Size(), len(want))
	}
	// A read of the whole block, and a read of every 7-octet window, must
	// agree with the block -- an Opener whose offsets were computed from the
	// record rather than the block reads a header line here.
	whole := make([]byte, file.Size())
	if _, err := io.ReadFull(io.NewSectionReader(file, 0, file.Size()), whole); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if !bytes.Equal(whole, want) {
		t.Errorf("read %q, want %q", whole, want)
	}
	for off := range int64(len(want)) {
		n := min(int64(7), int64(len(want))-off)
		buf := make([]byte, n)
		got, err := file.ReadAt(buf, off)
		if err != nil || int64(got) != n {
			t.Fatalf("ReadAt(%d) = %d, %v", off, got, err)
		}
		if !bytes.Equal(buf, want[off:off+n]) {
			t.Errorf("ReadAt(%d) = %q, want %q", off, buf, want[off:off+n])
		}
	}
	// At and past the end: (0, io.EOF), never a short read with a nil error.
	if n, err := file.ReadAt(make([]byte, 4), file.Size()); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt at EOF = %d, %v; want 0, io.EOF", n, err)
	}
	if n, err := file.ReadAt(make([]byte, 4), file.Size()-2); n != 2 || !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt across EOF = %d, %v; want 2, io.EOF", n, err)
	}

	// A zero-length block opens, and reads nothing.
	empty, err := o.OpenFile(names[1])
	if err != nil {
		t.Fatalf("OpenFile(empty): %v", err)
	}
	if empty.Size() != 0 {
		t.Errorf("Size() of an empty block = %d, want 0", empty.Size())
	}
	if n, err := empty.ReadAt(make([]byte, 1), 0); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt on an empty block = %d, %v; want 0, io.EOF", n, err)
	}
	if err := empty.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if _, err := o.OpenFile("/nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("OpenFile(missing) = %v, want fs.ErrNotExist", err)
	}
	if _, err := o.OpenFile("/"); !errors.Is(err, fs.ErrInvalid) {
		t.Errorf("OpenFile(/) = %v, want fs.ErrInvalid", err)
	}
}

// TestReadOnly: the whole write half refuses, and says so with one sentinel.
func TestReadOnly(t *testing.T) {
	f := openHandbuilt(t)
	name := f.Names()[0]
	for what, err := range map[string]error{
		"WriteFile":  f.WriteFile(name, []byte("x"), os.FileMode(0o644)),
		"MkDir":      f.MkDir("/d", os.FileMode(0o755)),
		"DeleteFile": f.DeleteFile(name),
		"DeleteDir":  f.DeleteDir("/"),
		"Rename":     f.Rename(name, "other"),
	} {
		if !errors.Is(err, ErrReadOnly) {
			t.Errorf("%s = %v, want ErrReadOnly", what, err)
		}
	}
}

// TestOpenRejectsNonWARC: Open frames the whole file, so a file that is not a
// WARC fails at Open rather than at the read that happens to reach the bad
// record. OpenReader must return an untyped nil interface on that path: a typed
// nil *FS inside a filesystem.Filesystem is not nil, and a caller checking the
// value rather than the error would be told it has a filesystem.
func TestOpenRejectsNonWARC(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"not a warc", "PK\x03\x04nope\r\n"},
		{"second record truncated", validRecord + "WARC/1.1\r\nWARC-Type: metadata\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Open(strings.NewReader(tc.in), int64(len(tc.in))); err == nil {
				t.Fatal("Open succeeded")
			}
			got, err := OpenReader(strings.NewReader(tc.in), int64(len(tc.in)))
			if err == nil {
				t.Fatal("OpenReader succeeded")
			}
			if got != nil {
				t.Errorf("OpenReader returned a non-nil filesystem (%T) with an error", got)
			}
		})
	}
}

// TestBlockReadFailure: an I/O failure while reading a block reaches the caller
// rather than being reported as a short block.
func TestBlockReadFailure(t *testing.T) {
	flaky := &flakyReaderAt{data: handbuiltWARC}
	f, err := Open(flaky, int64(len(handbuiltWARC)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	name := f.Names()[0]
	flaky.fail = true
	if _, err := f.ReadFile(name); err == nil {
		t.Error("ReadFile on a failing reader succeeded")
	}
	if _, err := f.Record(name); err == nil {
		t.Error("Record on a failing reader succeeded")
	}
	// A zero-length block needs no read at all, so it still answers.
	if b, err := f.ReadFile(f.Names()[1]); err != nil || len(b) != 0 {
		t.Errorf("ReadFile(empty block) = %q, %v; want no bytes and no error", b, err)
	}
}

// TestOpenEmptyFile: a file with no records is a filesystem with no entries, not
// an error. A zero-length WARC is what a crawler that fetched nothing leaves.
func TestOpenEmptyFile(t *testing.T) {
	f, err := Open(bytes.NewReader(nil), 0)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if names := f.Names(); len(names) != 0 {
		t.Errorf("Names() = %v, want none", names)
	}
	ents, err := f.ListDir("/")
	if err != nil || len(ents) != 0 {
		t.Errorf("ListDir(/) = %v, %v; want an empty listing", ents, err)
	}
}
