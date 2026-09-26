// SPDX-License-Identifier: BSD-3-Clause

// Package warc reads and writes WARC files (ISO 28500), the format a web crawl
// is archived in, in pure Go with no cgo and no dependency outside
// github.com/go-filesystems/interface.
//
// Three entry points, one per way a caller wants the file:
//
//	func NewReader(r io.Reader) (*Reader, error)                              // stream records
//	func NewWriter(w io.Writer) *Writer                                       // write records
//	func OpenReader(r io.ReaderAt, size int64) (filesystem.Filesystem, error) // one entry per record
//
// Open is OpenReader with the concrete *FS, for the three calls that are not on
// the interface: Names, Header and Record.
//
// # The format, and the two places a reader gets it wrong
//
// A record is
//
//	WARC/1.1 CRLF
//	Header-Name: value CRLF      (repeated)
//	CRLF
//	<exactly Content-Length octets>
//	CRLF CRLF
//
// The version line, the field names and every CRLF are ASCII; Content-Length is
// mandatory and counts octets. WARC-Type, WARC-Date (ISO 8601, UTC) and
// WARC-Record-ID (a URI in angle brackets) are mandatory too.
//
// Two properties of that framing are where implementations fail, and this
// package is built around both:
//
// THE TWO TRAILING CRLFS ARE PART OF THE RECORD, not a separator to be skipped.
// A reader that consumes whitespace until the next "WARC/" instead of reading
// exactly four octets accepts a truncated record as a whole one, and mis-frames
// the file whenever a block ends in a newline -- which, for a response record
// holding an HTTP message, is the usual case rather than the odd one. This
// package reads four octets and compares them.
//
// THE BLOCK IS OPAQUE. It may contain CRLF, NUL, a lone CR, bytes that are not
// text; nothing may scan it for a delimiter. This package reads exactly
// Content-Length octets and never looks inside. It also does not ALLOCATE
// Content-Length octets up front: the length comes from the file, so a
// seventy-octet header claiming an eight-exabyte block would otherwise be an
// out-of-memory panic rather than an error.
//
// # Plain .warc only
//
// A .warc.gz is not a gzipped .warc: it is a concatenation of one gzip MEMBER
// per record, so that an index can seek to a record and inflate that record
// alone. This package handles plain .warc. A caller with a single-member stream
// wraps it:
//
//	zr, err := gzip.NewReader(f)
//	r, err := warc.NewReader(zr)
//
// In this family that is not a gap in practice:
// github.com/go-filesystems/unarchive peels compression before it dispatches on
// format, so a .warc.gz arrives here already inflated. It IS a gap for a caller
// that wants per-record random access into a .warc.gz, because that needs the
// gzip member offsets, which this package does not compute.
//
// # The filesystem view, and how entries are named
//
// OpenReader presents the file as a flat, read-only directory with one regular
// file per record, whose contents are that record's content block. Opening reads
// only the header blocks; a block is read when a caller asks for it, which is why
// FS implements the optional filesystem.Opener and can answer a byte range out
// of a hundred-megabyte block without materialising it. Every mutating method
// returns ErrReadOnly.
//
// An entry is named
//
//	<5-digit ordinal>-<WARC-Type>[-<sanitised tail of WARC-Target-URI>]
//
//	00000-warcinfo
//	00001-response-one.html
//	00002-request-one.html
//	00003-resource-blob.bin
//
// WARC-Record-ID is the obvious unique key and it is the wrong filename. Its
// value is a URI in angle brackets -- "<urn:uuid:5a3b...>" in practice -- whose
// brackets and colons are illegal in a Windows filename and whose colon is the
// HFS path separator, so it has to be mangled; what survives the mangling is a
// UUID, which tells a person nothing and which they cannot predict from the
// archive in front of them. The target URI alone is worse, because it is not
// unique: a crawl of one page writes a request AND a response record under the
// same WARC-Target-URI, and a revisit record later under the same one again, so
// three entries would silently become one.
//
// The ordinal is the record's position in the file. It is unique by
// construction, so no two entries can collide whatever the headers say; it sorts
// the listing into crawl order under a plain lexical sort; and a person can count
// to it. It is NOT stable across files -- record 5 of a re-fetched crawl is a
// different record, and concatenating two WARCs renumbers the second -- and that
// is the price. A caller that needs an identity across files wants
// WARC-Record-ID, which is what it is for, and Record(name).RecordID() hands it
// over.
//
// # What it refuses
//
// The reader refuses a version line other than WARC/1.0 or WARC/1.1, a line not
// terminated by CRLF, a header line with no colon or a field name that is not a
// token, a Content-Length that is absent, repeated or not a decimal octet count,
// a missing WARC-Type, WARC-Date or WARC-Record-ID, a WARC-Date that is not a UTC
// timestamp, a record ID without angle brackets, and a trailer that is not two
// CRLFs. All of those satisfy errors.Is(err, ErrSyntax); a truncated file
// satisfies errors.Is(err, io.ErrUnexpectedEOF); an I/O failure is neither, so a
// caller can tell "this is not a WARC" from "the disk went away".
//
// It does NOT refuse an unregistered WARC-Type. §5.5 tells a reader to pass an
// unknown record type through rather than treat the file as broken, so refusing
// one would reject files a conforming writer may produce.
//
// The writer refuses rather than repairs, because each case it refuses would read
// back as something other than what the caller passed: a missing mandatory field,
// a field name that is not a token, a value containing CR, LF or NUL -- that one
// is header injection, and the writer is the only place it can be stopped -- a
// value with a leading or trailing SP or HT, which a reader is entitled to strip,
// and a Content-Length disagreeing with len(Block).
//
// # ⚠ WHERE THE FIXTURES CAME FROM, AND WHAT THAT COSTS IN CONFIDENCE
//
// Neither wget nor warcio was installed on the machine this package was written
// on, so there was no crawl to point at and no archive to hand. The corpus is
// therefore two files, and the reader should know which is which:
//
//   - testdata/handbuilt.warc is OUR OWN CONSTRUCTION. Its octets were laid out
//     from §5's grammar by testdata/gen_handbuilt.py, a script that writes CRLFs
//     with string concatenation. It cannot fail the way a real archive can: it
//     contains no crawl's idiosyncrasies, no truncated fetch, no header a real
//     crawler emits that we did not think to write.
//   - testdata/warcio.warc was written by warcio (Apache-2.0), the Python
//     implementation behind Webrecorder and the Common Crawl tooling, installed
//     into a throwaway virtualenv for the purpose. Its framing is not ours. Its
//     payloads are, so committing it redistributes nothing third-party.
//
// No specimen from a public archive is committed: what is freely downloadable is
// not automatically redistributable, and a licence we could not establish is a
// licence we do not act on.
//
// What compensates for that, and what does not:
//
//   - A round trip in BOTH directions. Reader∘Writer and Writer∘Reader are both
//     tested, and that is the weaker half: two halves of one package agree with
//     each other about a misunderstanding every time.
//   - Bytes laid out by hand from the specification and read back. This is what
//     stops the two halves agreeing privately, and it is why
//     testdata/handbuilt.warc is generated by a script rather than by the Writer.
//   - Byte equality with warcio. TestWriterReproducesWarcio reads
//     testdata/warcio.warc and re-emits it; the result must be identical, octet
//     for octet. It is. So our Writer puts every CRLF where an implementation we
//     did not write put it.
//   - Measured off-repository, not in CI, because warcio cannot be a CI
//     dependency: warcio reads both fixtures and both of our re-emissions, 7 and
//     4 records, every block the declared length. It agrees with us on the record
//     count, the types, the block lengths, the three repeated WARC-Concurrent-To
//     values and on splitting "X-Note: a:b:c: d" at the FIRST colon.
//
// The one place warcio and this package disagree is worth stating rather than
// hiding. For a folded field value
//
//	X-Folded: first CRLF HT second CRLF SP SP SP SP third
//
// this package yields "first second third" and warcio yields
// "first\tsecond    third". RFC 2616 §2.2, which ISO 28500 §4 takes LWS from,
// says a recipient MAY replace linear white space with a single SP before
// interpreting a value, so both readings conform. This package normalises,
// because a value's meaning should not depend on where a writer chose to break
// the line; a caller that needs the octets as they lay on disk should not be
// reading them through a field value at all.
//
// Fixtures are EMBEDDED with //go:embed rather than read from testdata/ at run
// time, because this repository's cross-architecture lanes build the test binary
// with `go test -c` and run it in a container holding the binary and nothing
// else. A test that read testdata/ would pass on the four native lanes and fail
// on the four emulated ones.
package warc
