# warc

A pure-Go reader and writer for **WARC** (ISO 28500), the format a web crawl is
archived in — plus a read-only `filesystem.Filesystem` view with one entry per
record.

No cgo. One dependency: `github.com/go-filesystems/interface`.

```go
import "github.com/go-filesystems/warc"

// Stream records.
r, err := warc.NewReader(f)
for {
    rec, err := r.Next()
    if errors.Is(err, io.EOF) {
        break
    }
    if err != nil {
        return err
    }
    fmt.Println(rec.Type(), rec.TargetURI(), len(rec.Block))
}

// Write records.
w := warc.NewWriter(out)
err = w.WriteRecord(&warc.Record{
    Header: warc.Header{
        {warc.HeaderType, warc.TypeResource},
        {warc.HeaderRecordID, "<urn:uuid:5a3b...>"},
        {warc.HeaderDate, "2026-09-26T06:00:00Z"},
        {warc.HeaderTargetURI, "http://example.org/a.txt"},
    },
    Block: []byte("hello"),
}) // Content-Length is computed and appended

// See the file as a filesystem: one file per record, contents = the block.
fsys, err := warc.OpenReader(ra, size)
ents, err := fsys.ListDir("/")
if o, ok := fsys.(filesystem.Opener); ok {
    file, err := o.OpenFile("00001-response-one.html") // random access, no full read
}
```

## The format, and the two traps

```
WARC/1.1 CRLF
Header-Name: value CRLF      (repeated)
CRLF
<exactly Content-Length octets>
CRLF CRLF
```

**The two trailing CRLFs are part of the record**, not a separator to trim. A
reader that skips whitespace until the next `WARC/` accepts a truncated record
as a whole one, and mis-frames the file whenever a block ends in a newline —
which for a `response` record carrying an HTTP message is the ordinary case.
This package reads four octets and compares them.

**The block is opaque.** It may contain CRLF, NUL, a lone CR, anything. Never
scan for a delimiter; read exactly `Content-Length` octets. This package also
does not *allocate* `Content-Length` up front — the length comes from the file,
so a seventy-octet header claiming an eight-exabyte block would otherwise be an
out-of-memory panic instead of an error.

## Plain `.warc` only

A `.warc.gz` is **not** a gzipped `.warc`: it is one gzip *member* per record,
concatenated, so an index can seek to a record and inflate it alone. This
package handles plain `.warc`; wrap the stream yourself for a single-member
file. In this family that is not a gap in practice —
`github.com/go-filesystems/unarchive` peels compression before dispatching on
format, so a `.warc.gz` arrives here already inflated. It **is** a gap for a
caller wanting per-record random access into a `.warc.gz`, which needs the gzip
member offsets this package does not compute.

## Entry names in the filesystem view

```
<5-digit ordinal>-<WARC-Type>[-<sanitised tail of WARC-Target-URI>]

00000-warcinfo
00001-response-one.html
00002-request-one.html
00003-resource-blob.bin
```

`WARC-Record-ID` is the obvious unique key and the wrong filename: its value is
a URI in angle brackets (`<urn:uuid:5a3b…>`) whose brackets and colons are
illegal on Windows and whose colon is the HFS path separator, so it must be
mangled — and what survives is a UUID nobody can predict from the archive in
front of them. The target URI alone is worse, because it is **not unique**: a
crawl of one page writes a request *and* a response under the same
`WARC-Target-URI`, and a revisit record later under the same one again, so three
entries would silently become one.

The ordinal is the record's position in the file: unique by construction, sorts
into crawl order under a plain lexical sort, and countable by a person. It is
**not** stable across files — a re-fetched crawl renumbers, and concatenating two
WARCs renumbers the second. A caller needing identity across files wants
`WARC-Record-ID`, which is what it is for, and `(*FS).Record(name).RecordID()`
hands it over.

Mutating methods return `ErrReadOnly`: `Content-Length` fixes each record's
extent, so changing one octet of a block either moves every later record or is
not the record that was written.

## Errors

| | |
|---|---|
| `errors.Is(err, warc.ErrSyntax)` | this is not a WARC, or not a valid one |
| `errors.Is(err, io.ErrUnexpectedEOF)` | the file is truncated |
| neither | an I/O failure, returned unwrapped |
| `errors.Is(err, fs.ErrNotExist)` | the filesystem view's missing-path contract |
| `errors.Is(err, warc.ErrReadOnly)` | every mutating method |

An **unregistered `WARC-Type` is accepted**, not refused: §5.5 tells a reader to
pass an unknown record type through rather than treat the file as broken.

## ⚠ Where the fixtures came from, and what that costs in confidence

Neither `wget` nor `warcio` was installed on the machine this was written on, so
there was no crawl to point at. The corpus is two files, and they are not equal
in standing:

- **`testdata/handbuilt.warc` is our own construction.** Its octets were laid
  out from §5's grammar by `testdata/gen_handbuilt.py`, which writes CRLFs with
  string concatenation — deliberately **not** with our `Writer`. It cannot fail
  the way a real archive can: no crawl's idiosyncrasies, no truncated fetch, no
  header a real crawler emits that we did not think to write.
- **`testdata/warcio.warc` was written by warcio** (Apache-2.0), the Python
  implementation behind Webrecorder and the Common Crawl tooling, installed into
  a throwaway virtualenv by `testdata/gen_warcio.py`. Its *framing* is not ours.
  Its payloads are, so committing it redistributes nothing third-party.

No specimen from a public archive is committed: freely downloadable is not the
same as redistributable, and a licence we could not establish is one we do not
act on.

What compensates:

- **A round trip in both directions** — and this is the weaker half, because two
  halves of one package agree with each other about a misunderstanding every
  time.
- **Bytes laid out by hand from the specification, read back.** This is what
  stops the two halves agreeing privately.
- **Byte equality with warcio.** `TestWriterReproducesWarcio` re-emits
  `testdata/warcio.warc` from the records our `Reader` parsed; the result is
  identical, octet for octet.
- **Measured off-repository** (warcio cannot be a CI dependency): warcio reads
  both fixtures and both of our re-emissions — 7 and 4 records, every block the
  declared length — and agrees on types, lengths, the three repeated
  `WARC-Concurrent-To` values and on splitting `X-Note: a:b:c: d` at the **first**
  colon.

One disagreement, stated rather than hidden: for a folded value
`X-Folded: first CRLF HT second`, this package yields `first second` and warcio
yields `first\tsecond`. RFC 2616 §2.2 — which ISO 28500 §4 takes LWS from — says
a recipient *may* replace linear white space with a single SP, so both conform.
We normalise, because a value's meaning should not depend on where a writer
broke the line.

## What the corpus is asserted to reach

`TestCorpusReaches` fails if any of these leaves the corpus, so a regenerated or
truncated fixture cannot quietly make the suite prove less: a block containing
CRLF, a block **ending** in CRLF, a block containing NUL, a zero-length block, a
field value containing a colon, an empty field value, a repeated field name, a
`WARC/1.0` record, an unregistered `WARC-Type`, and a folded field value.

## Ablations

Each row is a plausible wrong implementation, applied to a scratch copy and run
against the suite.

| Ablation | Result | Caught by |
|---|---|---|
| Skip whitespace at the trailer instead of consuming exactly CRLF CRLF | caught | 3 negative cases only — **every positive test still passed** |
| Scan for the CRLF CRLF delimiter instead of using `Content-Length` | caught | 12 tests, incl. both fixtures and the whole filesystem suite |
| Keep one value per field name (last wins, as a map does) | caught | 5 tests, incl. the repeated-header and two-`Content-Length` cases |
| Split a header line at the **last** colon | caught | 13 tests |
| Accept a bare LF as a line terminator | caught | 1 case (`LF-terminated version line`) |
| No LWS unfolding: a continuation line becomes its own field | caught | 10 tests |
| `strings.TrimSpace` instead of `strings.Trim(value, " \t")` | **PASSED** — see below | now `TestHeaderValueTrimsOnlySPandHT` |
| Drop the ordinal from the entry name (type + URI tail only) | caught | naming tests + 3 filesystem tests, on duplicate names |
| Refuse an unregistered `WARC-Type` (contra §5.5) | caught | 9 tests |
| Compare field names case-sensitively | caught | 2 header tests |
| `Writer` trims a value instead of refusing it | caught | 2 cases in `TestWriteRefuses` |

**The one that passed.** `strings.TrimSpace` strips VT, FF and every Unicode
space, U+00A0 among them; LWS is SP and HT and nothing else, so those octets are
ordinary field-content and a reader that eats them hands back a value missing
bytes the file carried, with nothing to say so. The whole suite passed under that
change. `TestHeaderValueTrimsOnlySPandHT` now pins it, with a value whose ends
are U+00A0, VT and FF inside real LWS.

The first row is worth reading twice: the trailer ablation was invisible to every
positive test, including the fixture whose block ends in CRLF, because reading by
count first and then skipping whitespace still lands in the right place. Only the
*negative* cases — a one-CRLF trailer, an LF LF trailer, a trailer eaten by a
block longer than its declared length — could see it.

## Testing

```
export GOWORK=off GOFLAGS=-mod=mod
go test -race ./...
```

CI runs 8 lanes — 4 native (linux/amd64, linux/arm64, darwin/arm64,
windows/amd64) and 4 emulated under QEMU (riscv64, loong64, ppc64le, s390x) —
behind a **100 % statement coverage gate**. Fixtures are `//go:embed`ed because
the emulated lanes run a `go test -c` binary in a container with no `testdata/`.

To regenerate the fixtures:

```
python3 testdata/gen_handbuilt.py testdata/handbuilt.warc
python3 -m venv /tmp/warcvenv && /tmp/warcvenv/bin/pip install warcio
/tmp/warcvenv/bin/python testdata/gen_warcio.py testdata/warcio.warc
```

Both are byte-for-byte reproducible: every record ID and date is fixed in the
scripts, so a regeneration that differs means the producer changed its framing,
which is worth knowing.

## Licence

BSD-3-Clause.
