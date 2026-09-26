#!/usr/bin/env python3
"""Regenerate warcio.warc with warcio, an outside WARC implementation.

This script exists so the fixture it produces is not our own construction.
warcio (https://github.com/webrecorder/warcio, Apache-2.0) is the reference
Python implementation used by Webrecorder and the Common Crawl tooling; a
fixture it wrote is a second opinion about the format, and our Reader either
agrees with it or one of the two is wrong.

The fixture's *payloads* are ours -- three short documents we made up -- so
nothing third-party is redistributed by committing the output. Only the framing
comes from warcio.

Usage:

    python3 -m venv /tmp/warcvenv
    /tmp/warcvenv/bin/pip install warcio
    /tmp/warcvenv/bin/python testdata/gen_warcio.py testdata/warcio.warc

The output is byte-for-byte reproducible: every record ID and date is fixed
below, so a regeneration that differs means warcio changed its framing, which
is worth knowing.
"""

import io
import sys

from warcio.statusandheaders import StatusAndHeaders
from warcio.warcwriter import WARCWriter

DATE = "2026-09-26T06:00:00Z"


def main(path):
    with open(path, "wb") as out:
        writer = WARCWriter(out, gzip=False, warc_version="WARC/1.1")

        info = writer.create_warcinfo_record(
            "warcio.warc",
            {
                "software": "warcio (fixture generator for go-filesystems/warc)",
                "format": "WARC File Format 1.1",
                "description": "hand-chosen payloads, warcio framing",
            },
        )
        info.rec_headers.replace_header("WARC-Date", DATE)
        info.rec_headers.replace_header(
            "WARC-Record-ID", "<urn:uuid:00000000-0000-4000-8000-000000000001>"
        )
        writer.write_record(info)

        body = b"<!doctype html>\r\n<title>one</title>\r\n<p>first page\r\n"
        http_headers = StatusAndHeaders(
            "200 OK",
            [
                ("Content-Type", "text/html; charset=utf-8"),
                ("Content-Length", str(len(body))),
            ],
            protocol="HTTP/1.1",
        )
        resp = writer.create_warc_record(
            "http://example.org/one.html",
            "response",
            payload=io.BytesIO(body),
            length=len(body),
            http_headers=http_headers,
        )
        resp.rec_headers.replace_header("WARC-Date", DATE)
        resp.rec_headers.replace_header(
            "WARC-Record-ID", "<urn:uuid:00000000-0000-4000-8000-000000000002>"
        )
        writer.write_record(resp)

        req_headers = StatusAndHeaders(
            "GET /one.html HTTP/1.1",
            [("Host", "example.org"), ("User-Agent", "fixture/1.0")],
            is_http_request=True,
        )
        req = writer.create_warc_record(
            "http://example.org/one.html",
            "request",
            http_headers=req_headers,
        )
        req.rec_headers.replace_header("WARC-Date", DATE)
        req.rec_headers.replace_header(
            "WARC-Record-ID", "<urn:uuid:00000000-0000-4000-8000-000000000003>"
        )
        writer.write_record(req)

        # A resource record whose block is binary and contains a NUL: the point
        # of the format is that the block is opaque, and a fixture of nothing
        # but text would never exercise it.
        blob = bytes(range(0, 32)) + b"\r\n\x00tail"
        res = writer.create_warc_record(
            "http://example.org/data/blob.bin",
            "resource",
            payload=io.BytesIO(blob),
            length=len(blob),
            warc_content_type="application/octet-stream",
        )
        res.rec_headers.replace_header("WARC-Date", DATE)
        res.rec_headers.replace_header(
            "WARC-Record-ID", "<urn:uuid:00000000-0000-4000-8000-000000000004>"
        )
        writer.write_record(res)


if __name__ == "__main__":
    main(sys.argv[1] if len(sys.argv) > 1 else "warcio.warc")
