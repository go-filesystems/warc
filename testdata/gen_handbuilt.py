#!/usr/bin/env python3
r"""Regenerate handbuilt.warc by emitting the octets ISO 28500 §5 specifies.

This file is built with string concatenation and explicit \r\n, from the
specification's grammar -- NOT with this repository's Writer. That is the whole
point of it. A fixture written by our Writer and read by our Reader proves the
two halves agree; it cannot show that they agree with the format, because a
shared misunderstanding passes such a test every time. These bytes are laid out
by hand so the Reader is tested against the specification instead.

    record       = version CRLF *named-field CRLF block CRLF CRLF
    version      = "WARC/1.1"
    named-field  = field-name ":" [ field-value ] CRLF
    field-value  = *( field-content | LWS )   ; LWS = [CRLF] 1*( SP | HT )

Deliberately covered, because each one is a way a plausible reader goes wrong:

  0  a block ending in CRLF                  -- a reader scanning for "\r\n\r\n"
                                                frames this one short
  1  a zero-length block                     -- Content-Length: 0 still carries
                                                the two trailing CRLFs
  2  a block containing NUL and a stray CR    -- the block is opaque
  3  a repeated field name                    -- two WARC-Concurrent-To
  4  a field value full of colons             -- split at the FIRST one only
  5  a folded (continuation) field value      -- LWS folding, unfolded to one SP
  6  WARC/1.0, and an unregistered WARC-Type  -- §5.5: pass unknown types through

Usage: python3 testdata/gen_handbuilt.py testdata/handbuilt.warc
"""

import sys

CRLF = b"\r\n"
DATE = b"2026-09-26T06:00:00Z"


def record(version, fields, block, content_length=None):
    """Emit one record. content_length overrides len(block) on purpose: the
    negative fixtures in the Go tests need that lever, and keeping it here means
    the framing is written in exactly one place."""
    out = bytearray()
    out += version + CRLF
    for name, value in fields:
        out += name + b": " + value + CRLF
    if content_length is None:
        content_length = len(block)
    out += b"Content-Length: " + str(content_length).encode() + CRLF
    out += CRLF
    out += block
    out += CRLF + CRLF
    return bytes(out)


def rid(n):
    return b"<urn:uuid:00000000-0000-4000-8000-%012d>" % n


def main(path):
    out = bytearray()

    # 0 -- a block that ends in CRLF. An HTTP message does, so this is the
    # ordinary case, not a contrived one.
    out += record(
        b"WARC/1.1",
        [
            (b"WARC-Type", b"response"),
            (b"WARC-Record-ID", rid(1)),
            (b"WARC-Date", DATE),
            (b"WARC-Target-URI", b"http://example.org/ends-in-crlf.html"),
            (b"Content-Type", b"application/http; msgtype=response"),
        ],
        b"HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\nend\r\n",
    )

    # 1 -- a zero-length block.
    out += record(
        b"WARC/1.1",
        [
            (b"WARC-Type", b"revisit"),
            (b"WARC-Record-ID", rid(2)),
            (b"WARC-Date", DATE),
            (b"WARC-Target-URI", b"http://example.org/ends-in-crlf.html"),
            (b"WARC-Profile", b"http://netpreserve.org/warc/1.1/revisit/identical-payload-digest"),
        ],
        b"",
    )

    # 2 -- an opaque block: NUL, a CR that is not part of a CRLF, an LF alone,
    # and a run of high bytes.
    out += record(
        b"WARC/1.1",
        [
            (b"WARC-Type", b"resource"),
            (b"WARC-Record-ID", rid(3)),
            (b"WARC-Date", DATE),
            (b"WARC-Target-URI", b"http://example.org/data/blob.bin?v=2#frag"),
            (b"Content-Type", b"application/octet-stream"),
        ],
        b"\x00\x01\x02\rnot-a-crlf\nlf-alone\x00\xff\xfe\xfd" + bytes(range(0xF0, 0x100)),
    )

    # 3 -- a repeated field name. WARC-Concurrent-To is the field the
    # specification repeats in practice.
    out += record(
        b"WARC/1.1",
        [
            (b"WARC-Type", b"metadata"),
            (b"WARC-Record-ID", rid(4)),
            (b"WARC-Date", DATE),
            (b"WARC-Concurrent-To", rid(1)),
            (b"WARC-Concurrent-To", rid(2)),
            (b"WARC-Concurrent-To", rid(3)),
            (b"Content-Type", b"application/warc-fields"),
        ],
        b"via: http://example.org/\r\nhopsFromSeed: 2\r\n",
    )

    # 4 -- a field value made of colons. Splitting at the last colon, or at
    # every colon, gives a different value for every one of these.
    out += record(
        b"WARC/1.1",
        [
            (b"WARC-Type", b"metadata"),
            (b"WARC-Record-ID", rid(5)),
            (b"WARC-Date", DATE),
            (b"WARC-Target-URI", b"https://example.org:8443/a:b/c?q=x:y#z:w"),
            (b"WARC-Payload-Digest", b"sha1:3I42H3S6NNFQ2MSVX7XZKYAYSCX5QBYJ"),
            (b"X-Note", b"a:b:c: d"),
            (b"X-Empty", b""),
        ],
        b"colons",
    )

    # 5 -- a folded field value. The grammar's LWS lets a value continue on the
    # next line when that line starts with SP or HT; the value means the same as
    # the unfolded one.
    folded = bytearray()
    folded += b"WARC/1.1" + CRLF
    folded += b"WARC-Type: metadata" + CRLF
    folded += b"WARC-Record-ID: " + rid(6) + CRLF
    folded += b"WARC-Date: " + DATE + CRLF
    folded += b"X-Folded: first" + CRLF
    folded += b"\tsecond" + CRLF
    folded += b"    third" + CRLF
    folded += b"Content-Length: 6" + CRLF
    folded += CRLF
    folded += b"folded"
    folded += CRLF + CRLF
    out += bytes(folded)

    # 6 -- WARC/1.0, and a WARC-Type no registry knows. §5.5 tells a reader to
    # pass an unknown type through, so refusing it would be a bug.
    out += record(
        b"WARC/1.0",
        [
            (b"WARC-Type", b"future-thing"),
            (b"WARC-Record-ID", rid(7)),
            (b"WARC-Date", DATE),
            (b"WARC-Target-URI", b"http://example.org/"),
        ],
        b"a type from 2040",
    )

    with open(path, "wb") as fh:
        fh.write(out)


if __name__ == "__main__":
    main(sys.argv[1] if len(sys.argv) > 1 else "handbuilt.warc")
