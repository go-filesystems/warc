// SPDX-License-Identifier: BSD-3-Clause

package warc

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestHeaderAccessors(t *testing.T) {
	h := Header{
		{HeaderType, "response"},
		{HeaderConcurrentTo, "<a>"},
		{"X-Note", ""},
		{HeaderConcurrentTo, "<b>"},
	}

	// Names are compared case-insensitively: the grammar's field-name is a
	// token, and a file that says "warc-type" means WARC-Type.
	for _, name := range []string{HeaderType, "warc-type", "WARC-TYPE"} {
		if got := h.Get(name); got != "response" {
			t.Errorf("Get(%q) = %q, want %q", name, got, "response")
		}
		if !h.Has(name) {
			t.Errorf("Has(%q) = false", name)
		}
	}
	if got := h.Get("X-Absent"); got != "" {
		t.Errorf("Get(absent) = %q, want \"\"", got)
	}
	if h.Has("X-Absent") {
		t.Error("Has(absent) = true")
	}

	// Get cannot tell an absent field from an empty one; Has can. That is the
	// whole reason Has exists.
	if h.Get("X-Note") != "" || !h.Has("X-Note") {
		t.Error("Get/Has disagree about a present field with an empty value")
	}

	if got := h.Values(HeaderConcurrentTo); !slices.Equal(got, []string{"<a>", "<b>"}) {
		t.Errorf("Values(repeated) = %v, want [<a> <b>]", got)
	}
	if got := h.Values("X-Absent"); got != nil {
		t.Errorf("Values(absent) = %v, want nil", got)
	}
}

func TestHeaderMutators(t *testing.T) {
	var h Header
	h.Add(HeaderType, "response")
	h.Add(HeaderConcurrentTo, "<a>")
	h.Add(HeaderConcurrentTo, "<b>")
	h.Add(HeaderConcurrentTo, "<c>")
	if got := h.Values(HeaderConcurrentTo); !slices.Equal(got, []string{"<a>", "<b>", "<c>"}) {
		t.Fatalf("after Add: %v", got)
	}

	// Set replaces the first and drops the rest, IN PLACE: the surviving field
	// keeps its position, because a WARC's header order is part of what was
	// read.
	h.Add("X-Last", "z")
	h.Set("warc-concurrent-to", "<only>")
	want := Header{{HeaderType, "response"}, {HeaderConcurrentTo, "<only>"}, {"X-Last", "z"}}
	if !slices.Equal(h, want) {
		t.Errorf("after Set: %v, want %v", h, want)
	}

	// Set on an absent name appends.
	h.Set("X-New", "n")
	if h.Get("X-New") != "n" || len(h) != 4 || h[3].Name != "X-New" {
		t.Errorf("Set(absent) = %v", h)
	}

	h.Del("x-new")
	h.Del(HeaderConcurrentTo)
	if h.Has("X-New") || h.Has(HeaderConcurrentTo) || len(h) != 2 {
		t.Errorf("after Del: %v", h)
	}
	h.Del("X-Absent")
	if len(h) != 2 {
		t.Errorf("Del(absent) changed the header: %v", h)
	}
}

func TestHeaderClone(t *testing.T) {
	if got := Header(nil).Clone(); got != nil {
		t.Errorf("nil.Clone() = %v, want nil", got)
	}
	h := Header{{HeaderType, "response"}}
	c := h.Clone()
	c[0].Value = "request"
	if h[0].Value != "response" {
		t.Error("Clone shares storage with the original")
	}
}

func TestRecordAccessors(t *testing.T) {
	rec := &Record{
		Version: Version11,
		Header: Header{
			{HeaderType, "response"},
			{HeaderRecordID, "<urn:uuid:2f7e>"},
			{HeaderDate, "2026-09-26T06:00:00.25Z"},
			{HeaderTargetURI, "http://example.org/a"},
			{HeaderContentType, "application/http; msgtype=response"},
		},
	}
	if rec.Type() != "response" {
		t.Errorf("Type() = %q", rec.Type())
	}
	// The brackets stay: the value is "<uri>", and handing back "uri" would
	// not be what the file says.
	if rec.RecordID() != "<urn:uuid:2f7e>" {
		t.Errorf("RecordID() = %q, want the angle brackets kept", rec.RecordID())
	}
	if rec.TargetURI() != "http://example.org/a" {
		t.Errorf("TargetURI() = %q", rec.TargetURI())
	}
	if rec.ContentType() != "application/http; msgtype=response" {
		t.Errorf("ContentType() = %q", rec.ContentType())
	}
	got, err := rec.Date()
	if err != nil {
		t.Fatalf("Date: %v", err)
	}
	// WARC 1.1 permits fractional seconds; 1.0 did not. Both are accepted, so
	// the fraction must survive.
	if want := time.Date(2026, 9, 26, 6, 0, 0, 250_000_000, time.UTC); !got.Equal(want) {
		t.Errorf("Date() = %v, want %v", got, want)
	}
	if got.Location() != time.UTC {
		t.Errorf("Date() location = %v, want UTC", got.Location())
	}

	// An empty Record answers without panicking, and its Date complains.
	empty := &Record{}
	if empty.Type() != "" || empty.RecordID() != "" || empty.TargetURI() != "" || empty.ContentType() != "" {
		t.Error("an empty Record's accessors did not return empty strings")
	}
	if _, err := empty.Date(); !errors.Is(err, ErrSyntax) {
		t.Errorf("empty.Date() = %v, want an ErrSyntax", err)
	}
}

func TestParseWARCDate(t *testing.T) {
	for _, tc := range []struct {
		in   string
		ok   bool
		want string
	}{
		{"2026-09-26T06:00:00Z", true, ""},
		{"2026-09-26T06:00:00.123456Z", true, ""},
		{"", false, "not a UTC W3C-DTF timestamp"},
		{"2026-09-26T06:00:00", false, "not a UTC W3C-DTF timestamp"},
		{"2026-09-26T06:00:00+00:00", false, "not a UTC W3C-DTF timestamp"},
		{"2026-09-26 06:00:00Z", false, "cannot parse"},
		{"2026-13-99T06:00:00Z", false, "month out of range"},
		{"Z", false, "cannot parse"},
	} {
		_, err := parseWARCDate(tc.in)
		if tc.ok {
			if err != nil {
				t.Errorf("parseWARCDate(%q) = %v, want no error", tc.in, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("parseWARCDate(%q) succeeded, want %q", tc.in, tc.want)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseWARCDate(%q) = %v, want it to contain %q", tc.in, err, tc.want)
		}
		if !errors.Is(err, ErrSyntax) {
			t.Errorf("parseWARCDate(%q) error does not satisfy errors.Is(err, ErrSyntax)", tc.in)
		}
	}
}

func TestParseContentLength(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
		ok   bool
	}{
		{"0", 0, true},
		{"1", 1, true},
		{"9223372036854775807", 1 << 62, false}, // in range, checked below
		{"", 0, false},
		{" 1", 0, false},
		{"1 ", 0, false},
		{"+1", 0, false},
		{"-1", 0, false},
		{"0x10", 0, false},
		{"1_0", 0, false},
		{"one", 0, false},
		{"99999999999999999999", 0, false}, // 20 digits: out of int64 range
	} {
		got, err := parseContentLength(tc.in)
		switch {
		case tc.in == "9223372036854775807":
			if err != nil || got != 1<<63-1 {
				t.Errorf("parseContentLength(MaxInt64) = %d, %v", got, err)
			}
		case tc.ok:
			if err != nil || got != tc.want {
				t.Errorf("parseContentLength(%q) = %d, %v; want %d", tc.in, got, err, tc.want)
			}
		default:
			if err == nil {
				t.Errorf("parseContentLength(%q) = %d, want an error", tc.in, got)
			} else if !errors.Is(err, ErrSyntax) {
				t.Errorf("parseContentLength(%q) error is not an ErrSyntax: %v", tc.in, err)
			}
		}
	}
}

func TestIsToken(t *testing.T) {
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{"WARC-Type", true},
		{"X", true},
		{"x.y_z-1", true},
		{"", false},
		{"a b", false},       // SP: the line would parse as a different field
		{"a:b", false},       // COLON: the line would split in the wrong place
		{"a\tb", false},      // HT
		{"a\rb", false},      // CR
		{"a(b", false},       // an RFC 2616 separator
		{"a\x7fb", false},    /* DEL */
		{"a\xc3\xa9", false}, // non-ASCII
	} {
		if got := isToken(tc.in); got != tc.ok {
			t.Errorf("isToken(%q) = %v, want %v", tc.in, got, tc.ok)
		}
	}
}

// TestErrSyntaxIsNotEverything: the sentinel must not match errors it has
// nothing to do with, or errors.Is(err, ErrSyntax) tells a caller nothing.
func TestErrSyntaxIsNotEverything(t *testing.T) {
	err := errorf("something")
	if !errors.Is(err, ErrSyntax) {
		t.Error("an ErrSyntax does not satisfy errors.Is(err, ErrSyntax)")
	}
	if errors.Is(err, ErrReadOnly) {
		t.Error("an ErrSyntax also satisfies errors.Is(err, ErrReadOnly)")
	}
	if !strings.HasPrefix(err.Error(), "warc: ") {
		t.Errorf("error %q is not prefixed with the package name", err)
	}
}
