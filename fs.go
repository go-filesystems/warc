// SPDX-License-Identifier: BSD-3-Clause

package warc

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	filesystem "github.com/go-filesystems/interface"
)

// Mode bits for the two kinds of thing this filesystem has. filesystem.Stat's
// Mode is a uint16 in the POSIX st_mode encoding, so the file-type bits go in
// the top nibble.
const (
	modeDir = 0o040000 | 0o555 // S_IFDIR, r-xr-xr-x
	modeReg = 0o100000 | 0o444 // S_IFREG, r--r--r--

	dtDir = 4 // DT_DIR
	dtReg = 8 // DT_REG
)

// rootInode is 1, as a root usually is; record i gets inode i+2, so no entry
// can collide with it.
const rootInode = 1

// entry is one record as the filesystem sees it: a name, the header we parsed,
// and where in the file its block is. The BLOCK IS NOT HELD: a WARC of a crawl
// is routinely gigabytes, and holding every block would make Open cost as much
// as the file. Only the offset and length are kept, and Opener reads on demand.
type entry struct {
	name     string
	ordinal  int
	version  string
	hdr      Header
	blockOff int64
	blockLen int64
}

// FS is a WARC file seen as a read-only filesystem: a flat directory with one
// regular file per record, whose contents are that record's content block.
//
// # Contents are the BLOCK, and the header is beside it
//
// Reading an entry gives the record's content block, not the record's bytes.
// That is the choice a caller of a filesystem wants -- the archived HTTP
// message, the fetched resource -- and it is what makes Stat's size the number
// ReadFile returns. The named fields are not lost: Record and Header on this
// type hand them back for any entry, and Reader gives both together for a
// caller that wants to stream.
//
// # Every mutating method returns ErrReadOnly
//
// A WARC is a record of what a crawl observed, and the format has no way to
// rewrite a record in place: Content-Length fixes each record's extent, so
// changing one byte of a block either changes the length -- moving every later
// record -- or is not the record that was written. So the write half of
// filesystem.Filesystem returns ErrReadOnly rather than a partial
// implementation.
type FS struct {
	r       io.ReaderAt
	entries []entry
	byName  map[string]int
}

// Open reads the record headers of the WARC file in the first size bytes of r
// and returns it as a filesystem. Every record is framed on the way through, so
// Open fails on a file that is not a WARC rather than on the read that reaches
// the bad record.
//
// Blocks are not read. Open costs one pass over the header blocks plus a seek
// per record; ReadFile and OpenFile read a block when asked.
func Open(r io.ReaderAt, size int64) (*FS, error) {
	rd := newReader(io.NewSectionReader(r, 0, size))
	f := &FS{r: r, byName: make(map[string]int)}
	for i := 0; ; i++ {
		rec, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		e := entry{
			name:     EntryName(i, rec.Type(), rec.TargetURI()),
			ordinal:  i,
			version:  rec.Version,
			hdr:      rec.Header,
			blockOff: rd.blockOff,
			blockLen: rd.blockLen,
		}
		f.byName[e.name] = len(f.entries)
		f.entries = append(f.entries, e)
	}
	return f, nil
}

// OpenReader opens the WARC file in the first size bytes of r and returns it as
// a filesystem.Filesystem.
//
// It exists so every driver in go-filesystems answers to ONE name for the same
// thing: github.com/go-filesystems/detect registers a
// func(io.ReaderAt, int64) (filesystem.Filesystem, error), and Open here has the
// right shape but the concrete return type, which does not fit. The concrete
// type is still available from Open for a caller that wants Record, Header or
// Names.
func OpenReader(r io.ReaderAt, size int64) (filesystem.Filesystem, error) {
	f, err := Open(r, size)
	if err != nil {
		// A typed nil inside an interface is not nil, so the error path
		// returns an untyped nil: a caller that checks the value rather
		// than the error would otherwise be told it has a filesystem.
		return nil, err
	}
	return f, nil
}

// EntryName is the name record ordinal (0-based) gets in the filesystem view:
//
//	00000-warcinfo
//	00001-request-index.html
//	00002-response-index.html
//	00003-resource-logo.png
//
// # Why not WARC-Record-ID
//
// The obvious unique key is WARC-Record-ID, and it is the wrong filename. Its
// value is a URI in angle brackets, in practice "<urn:uuid:5a3b...>": the angle
// brackets and the colons are not allowed in a Windows filename and the colon
// is the HFS path separator, so the name has to be mangled anyway; what is left
// is a UUID, which tells a person nothing and which they cannot predict from the
// archive they are looking at. A tool that wants the record ID can ask for it --
// Record(name).RecordID() -- and a person reading a directory listing gains
// nothing from seeing thirty UUIDs.
//
// The target URI alone is worse: it is not unique. A crawl of one page writes a
// request AND a response record with the same WARC-Target-URI, and a revisit
// record later with the same one again, so the names would collide -- silently,
// three entries becoming one.
//
// # Why ordinal first
//
// The ordinal is the record's position in the file. It is unique by
// construction, so no two entries can collide whatever the headers say; it
// sorts the listing into archive order, which is crawl order, under a plain
// lexical sort; and it is stable for a given file, which a hash of the record
// would also be but an ordinal is one a person can count to.
//
// It is NOT stable across files: record 5 of a WARC is not record 5 of the same
// crawl re-fetched, and concatenating two WARCs renumbers the second. That is
// the cost, and it is the right one to pay here -- a filesystem view of one file
// needs names unique within that file, and a caller that needs an identity
// across files has WARC-Record-ID, which is exactly what it is for.
//
// The type and the URI tail follow so the name says what the entry is. Both are
// sanitised to ASCII letters, digits, '.', '_' and '-' and truncated, so the
// name is safe on every filesystem this family writes to; two records whose
// tails sanitise to the same string still differ by ordinal.
func EntryName(ordinal int, warcType, targetURI string) string {
	name := fmt.Sprintf("%05d-%s", ordinal, sanitise(strings.ToLower(warcType), 32, "record"))
	if tail := uriTail(targetURI); tail != "" {
		name += "-" + tail
	}
	return name
}

// uriTail picks the part of a target URI worth putting in a filename: the last
// non-empty path segment, or the authority when the path has none (so
// "http://example.org/" gives "example.org" rather than nothing).
func uriTail(uri string) string {
	if uri == "" {
		return ""
	}
	s := uri
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	authority, rest, _ := strings.Cut(s, "/")
	segments := strings.Split(rest, "/")
	for i := len(segments) - 1; i >= 0; i-- {
		if segments[i] != "" {
			return sanitise(segments[i], 48, "")
		}
	}
	return sanitise(authority, 48, "")
}

// sanitise keeps the characters that are safe in a filename on every platform
// this family targets, folds every run of anything else to a single '_', and
// truncates. An empty result becomes fallback.
func sanitise(s string, max int, fallback string) string {
	var b strings.Builder
	lastUnderscore := false
	for i := 0; i < len(s) && b.Len() < max; i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-', c == '_':
			b.WriteByte(c)
			lastUnderscore = false
		case lastUnderscore:
		default:
			b.WriteByte('_')
			lastUnderscore = true
		}
	}
	out := strings.Trim(b.String(), "._-")
	if out == "" {
		return fallback
	}
	return out
}

// Names returns every entry name, in archive order.
func (f *FS) Names() []string {
	out := make([]string, len(f.entries))
	for i, e := range f.entries {
		out[i] = e.name
	}
	return out
}

// Header returns the named fields of the record behind an entry, in file order.
// The returned Header is a copy.
func (f *FS) Header(name string) (Header, error) {
	e, err := f.entry(name)
	if err != nil {
		return nil, err
	}
	return e.hdr.Clone(), nil
}

// Record returns the whole record behind an entry: version, headers and block.
// The block is read now.
func (f *FS) Record(name string) (*Record, error) {
	e, err := f.entry(name)
	if err != nil {
		return nil, err
	}
	block, err := f.readBlock(e)
	if err != nil {
		return nil, err
	}
	return &Record{Version: e.version, Header: e.hdr.Clone(), Block: block}, nil
}

// Close releases nothing: FS does not own the io.ReaderAt it was handed, and
// closing what a caller passed in would be closing something it may still be
// using. It is here because filesystem.Filesystem has it.
func (f *FS) Close() error { return nil }

// ReadFile returns the content block of the record at path.
func (f *FS) ReadFile(p string) ([]byte, error) {
	e, err := f.entry(p)
	if err != nil {
		return nil, err
	}
	return f.readBlock(e)
}

// readBlock materialises one block. io.ReadFull over a section of the file, not
// a bare ReadAt, because ReadAt is allowed to return fewer bytes with an error
// and a caller of ReadFile expects the whole block or a failure; for a
// zero-length block ReadFull returns (0, nil) without touching the reader.
func (f *FS) readBlock(e *entry) ([]byte, error) {
	buf := make([]byte, e.blockLen)
	sr := io.NewSectionReader(f.r, e.blockOff, e.blockLen)
	if _, err := io.ReadFull(sr, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// ListDir lists the root, the only directory this filesystem has.
func (f *FS) ListDir(p string) ([]filesystem.DirEntry, error) {
	clean := cleanPath(p)
	if clean != "/" {
		if _, err := f.entry(clean); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("warc: %q is not a directory: %w", clean, fs.ErrInvalid)
	}
	out := make([]filesystem.DirEntry, len(f.entries))
	for i, e := range f.entries {
		out[i] = filesystem.NewDirEntry(uint64(i)+rootInode+1, e.name, dtReg)
	}
	return out, nil
}

// Stat describes the root or one entry.
func (f *FS) Stat(p string) (filesystem.Stat, error) {
	clean := cleanPath(p)
	if clean == "/" {
		return filesystem.NewStat(modeDir, 0, rootInode), nil
	}
	e, err := f.entry(clean)
	if err != nil {
		return nil, err
	}
	return filesystem.NewStat(modeReg, uint64(e.blockLen), uint64(e.ordinal)+rootInode+1), nil
}

// ReadLink always fails: a WARC has no symbolic links. A path that is not there
// is still reported as not there, so a caller can tell the two apart.
func (f *FS) ReadLink(p string) (string, error) {
	clean := cleanPath(p)
	if clean != "/" {
		if _, err := f.entry(clean); err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("warc: %q is not a symbolic link: %w", clean, fs.ErrInvalid)
}

// OpenFile opens a record's content block for random access, which is the
// reason this driver implements filesystem.Opener: a response record's block can
// be a hundred megabytes, and a server answering a range request out of a WARC
// must not have to materialise all of it. Opening reads nothing.
func (f *FS) OpenFile(p string) (filesystem.File, error) {
	e, err := f.entry(p)
	if err != nil {
		return nil, err
	}
	return &File{sr: io.NewSectionReader(f.r, e.blockOff, e.blockLen)}, nil
}

// entry resolves a path to an entry, reporting fs.ErrNotExist for a name that
// is not there and fs.ErrInvalid for the root, which is not a regular file.
func (f *FS) entry(p string) (*entry, error) {
	clean := cleanPath(p)
	if clean == "/" {
		return nil, fmt.Errorf("warc: %q is a directory: %w", clean, fs.ErrInvalid)
	}
	i, ok := f.byName[strings.TrimPrefix(clean, "/")]
	if !ok {
		return nil, fmt.Errorf("warc: %q not found: %w", p, fs.ErrNotExist)
	}
	return &f.entries[i], nil
}

// cleanPath normalises a caller's path into this filesystem's flat namespace:
// "x", "/x", "./x" and "/a/../x" all name the same entry, and every form of the
// root becomes "/".
func cleanPath(p string) string {
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return path.Clean(p)
}

// File is an open content block: random access over the record's extent in the
// underlying file, and its size.
//
// ReadAt is io.SectionReader's, so it keeps io.ReaderAt's contract to the
// letter -- a short read always carries an error, an offset at or past the end
// gives (0, io.EOF), and concurrent calls are safe -- rather than this package
// reimplementing it and getting one of those wrong.
type File struct {
	sr *io.SectionReader
}

// ReadAt implements io.ReaderAt over the block.
func (f *File) ReadAt(p []byte, off int64) (int, error) { return f.sr.ReadAt(p, off) }

// Size is the block's length in octets, which is the record's Content-Length.
func (f *File) Size() int64 { return f.sr.Size() }

// Close releases nothing; a File holds an offset and a length.
func (f *File) Close() error { return nil }

// The write half of filesystem.Filesystem. Each one refuses; see FS.

// WriteFile always returns ErrReadOnly.
func (f *FS) WriteFile(p string, data []byte, perm os.FileMode) error { return ErrReadOnly }

// MkDir always returns ErrReadOnly.
func (f *FS) MkDir(p string, perm os.FileMode) error { return ErrReadOnly }

// DeleteFile always returns ErrReadOnly.
func (f *FS) DeleteFile(p string) error { return ErrReadOnly }

// DeleteDir always returns ErrReadOnly.
func (f *FS) DeleteDir(p string) error { return ErrReadOnly }

// Rename always returns ErrReadOnly.
func (f *FS) Rename(oldPath, newPath string) error { return ErrReadOnly }

// compile-time proof of the two interfaces this driver claims.
var (
	_ filesystem.Filesystem = (*FS)(nil)
	_ filesystem.Opener     = (*FS)(nil)
	_ filesystem.File       = (*File)(nil)
)
