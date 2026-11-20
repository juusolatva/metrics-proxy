package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// Fuzz targets for the parsers and security checks (AGENTS.md backlog item
// 2). go test runs only the seed corpus: the f.Add seeds plus any failing
// inputs committed under testdata/fuzz/<FuzzName>/. To fuzz one target:
//
//	go test -run '^$' -fuzz=FuzzCaptureSplit -fuzztime=30s

// captureKinds are the Content-Types newCapture handles, indexed by the
// fuzzed kind byte.
var captureKinds = []string{"application/json", "text/event-stream", "application/x-ndjson"}

// captureSeeds are the testdata/ captures and their capture kind.
var captureSeeds = []struct {
	file string
	kind byte
}{
	{"lm_v1_chat.json", 0}, {"lm_oai.json", 0}, {"ol_chat.json", 0}, {"ol_generate.json", 0}, {"ol_oai.json", 0},
	{"lm_v1_chat_stream.sse", 1}, {"lm_oai_stream.sse", 1}, {"lm_oai_stream_nousage.sse", 1},
	{"ol_oai_stream.sse", 1}, {"ol_oai_stream_nousage.sse", 1},
	{"ol_chat_stream.ndjson", 2}, {"ol_generate_stream.ndjson", 2},
}

// FuzzCaptureSplit checks that a body written in chunks gives the same
// metrics and diagnostics as the same body written at once, whatever the
// chunk boundaries, and that parsing never panics. Each byte of cuts is the
// size of the next chunk (0 counts as 1), cycling until the body is used up.
func FuzzCaptureSplit(f *testing.F) {
	for _, s := range captureSeeds {
		body := readFixture(f, s.file)
		f.Add(s.kind, body, []byte{1})
		f.Add(s.kind, body, []byte{7, 1, 200, 3})
		// The same body as a different kind: SSE as NDJSON and so on.
		f.Add((s.kind+1)%3, body, []byte{13})
	}
	f.Add(byte(1), []byte("data: {\"model\":\"m\"}\r\n\r\ndata: [DONE]\r\n\r\n"), []byte{1})
	f.Add(byte(2), []byte("{\"model\":\"m\"}\r\n\r\n  \n{\"eval_count\":3}"), []byte{2})

	f.Fuzz(func(t *testing.T, kind byte, body, cuts []byte) {
		contentType := captureKinds[int(kind)%len(captureKinds)]
		whole := newCapture(contentType)
		whole.Write(body)
		wantData, wantDiag := whole.finish(), whole.diag()

		split := newCapture(contentType)
		for i, b := 0, body; len(b) > 0; i++ {
			n := 1
			if len(cuts) > 0 {
				n = max(int(cuts[i%len(cuts)]), 1)
			}
			n = min(n, len(b))
			split.Write(b[:n])
			b = b[n:]
		}
		gotData, gotDiag := split.finish(), split.diag()

		if gotData != wantData {
			t.Errorf("%s split by %v: metrics %+v, whole body gives %+v", contentType, cuts, gotData, wantData)
		}
		if gotDiag != wantDiag {
			t.Errorf("%s split by %v: diag %+v, whole body gives %+v", contentType, cuts, gotDiag, wantDiag)
		}
	})
}

// FuzzEscapeControls checks that escaped text has nothing left to escape,
// that text with nothing to escape is unchanged, and that the escaping is
// lossless. Backslashes and double quotes are kept as they are, so only
// input without them unquotes back to itself.
func FuzzEscapeControls(f *testing.F) {
	for _, s := range []string{
		"", "plain", "\x1b[31mred\x1b[0m", "line\nbreak", "\u202eevil", " ", "\xff\xfe",
		"emoji \U0001F468\u200d\U0001F469", "\x7f\u0085\u009b", `back\slash "quoted"`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		got := escapeControls(in)
		if unsafeInLog(got) {
			t.Fatalf("escapeControls(%q) = %q, still contains characters to escape", in, got)
		}
		if !unsafeInLog(in) && got != in {
			t.Fatalf("escapeControls(%q) = %q, want it unchanged", in, got)
		}
		if strings.ContainsAny(in, `\"`) {
			return
		}
		if back, err := strconv.Unquote(`"` + got + `"`); err != nil || back != in {
			t.Fatalf("escapeControls(%q) = %q, unquotes to %q (%v)", in, got, back, err)
		}
	})
}

// FuzzCSVText checks that csvText only ever prepends an apostrophe, that the
// cell never starts with a formula prefix past the characters spreadsheets
// skip, and that the cell survives a CSV write and read unchanged.
func FuzzCSVText(f *testing.F) {
	for _, s := range []string{
		"", "qwen3:4b", "=1+1", "+cmd", "-2", "@SUM(A1)", " \t=x", "\u200b=x", "\x00=x", "＝x", "\xff", "a,b\"c\nd",
		"/v1;=1", "=a;-b;c", ";", "a;'=1", `a;"=1`, "a;\x00", "a;;b", "'0\xff",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		cell := csvText(s)
		if !onlyGuardsAdded(s, cell) {
			t.Fatalf("csvText(%q) = %q, want the value with apostrophes added only at the start and after \";\"", s, cell)
		}
		// Excel splits cells at ";" in some locales, so each part counts.
		for part := range strings.SplitSeq(cell, ";") {
			trimmed := strings.TrimLeftFunc(part, func(r rune) bool {
				return unicode.IsSpace(r) || unicode.IsControl(r) || unicode.In(r, unicode.Cf)
			})
			if strings.ContainsRune("=+-@＝＋－＠", firstRune(trimmed)) {
				t.Fatalf("csvText(%q) = %q, part %q starts with a formula prefix", s, cell, part)
			}
		}

		var buf bytes.Buffer
		w := csv.NewWriter(&buf)
		_ = w.Write([]string{cell, "next"}) // a bytes.Buffer write can't fail
		w.Flush()
		rec, err := csv.NewReader(&buf).Read()
		if err != nil {
			t.Fatalf("csvText(%q) = %q: reading the CSV row back: %v", s, cell, err)
		}
		// encoding/csv reads "\r\n" inside a quoted field as "\n".
		want := strings.ReplaceAll(cell, "\r\n", "\n")
		if len(rec) != 2 || rec[0] != want {
			t.Fatalf("csvText(%q) = %q, reads back from CSV as %q", s, cell, rec)
		}
	})
}

// onlyGuardsAdded reports whether cell is s with apostrophes inserted at
// the start or right after a ";", and nothing else changed. An apostrophe at
// such a place may be inserted or s's own, so both readings are tried; a
// wrong one fails within a character or two.
func onlyGuardsAdded(s, cell string) bool {
	return guardsMatch(s, cell, true)
}

// guardsMatch is onlyGuardsAdded for the rest of s and cell; partStart says
// whether cell is at the start of a part (the start of the value or just
// after a ";").
func guardsMatch(s, cell string, partStart bool) bool {
	if cell == "" {
		return s == ""
	}
	if partStart && cell[0] == '\'' && guardsMatch(s, cell[1:], false) {
		return true
	}
	return s != "" && cell[0] == s[0] && guardsMatch(s[1:], cell[1:], cell[0] == ';')
}

// firstRune returns the first rune of s, or -1 if s is empty.
func firstRune(s string) rune {
	for _, r := range s {
		return r
	}
	return -1
}

// FuzzHostName checks that when hostName gives a well-formed name (an IP
// address, or a DNS name of lowercase letters, digits, dots and hyphens
// without a trailing dot), a second pass leaves it alone. Malformed Host
// values such as "[:" or "[a:1]:2" may change again: each pass strips what
// looks like a port or brackets. That is harmless, since hostName runs once
// per request and browsers, which DNS rebinding goes through, only send
// DNS names and bracketed IPv6 literals.
func FuzzHostName(f *testing.F) {
	for _, s := range []string{
		"", "localhost", "LOCALHOST:11436", "127.0.0.1:80", "[::1]:11436", "[::1]", "::1",
		"example.com.", "evil.localhost.", "a.local:1", "host:port:extra", "[fe80::1%eth0]:1",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, hostport string) {
		h := hostName(hostport)
		if !wellFormedHost(h) {
			return
		}
		if again := hostName(h); again != h {
			t.Fatalf("hostName(%q) = %q, but hostName(%q) = %q", hostport, h, h, again)
		}
	})
}

// wellFormedHost reports whether h is an IP address (with a zone of
// interface-name characters, if any; netip accepts any zone, "]" included)
// or a lowercase DNS name (letters, digits, dots and hyphens, not ending in
// a dot).
func wellFormedHost(h string) bool {
	const name = "abcdefghijklmnopqrstuvwxyz0123456789.-"
	if a, err := netip.ParseAddr(h); err == nil {
		return strings.Trim(a.Zone(), name+"_") == ""
	}
	if h == "" || strings.HasSuffix(h, ".") {
		return false
	}
	return strings.Trim(h, name) == ""
}

// FuzzPathChecks checks the management path checks against an independent
// model of a lenient upstream router: one that reads backslashes as
// slashes, collapses repeated slashes, resolves "." and ".." segments and
// matches case-insensitively. No path that such a router sees as a
// management endpoint may pass both hasDotDotSegment and
// isManagementEndpoint, and being one must not change under the variations
// the router ignores.
func FuzzPathChecks(f *testing.F) {
	for _, p := range []string{
		"/api/pull", "/API/Pull", "//api//pull/", `\api\pull`, "/api/./pull", "/api/x/../pull",
		"/api/blobs/sha256:abc", "/api/blobs/..", "/api/v1/models/load", "/api/v1/chat", "/v1/chat/completions",
		"api/pull", "/api/pulls", "/api/user/keys/1", "/./api/me", "/api/me/..%2f", "",
	} {
		f.Add(p)
	}
	f.Fuzz(func(t *testing.T, p string) {
		dotDot, mgmt := hasDotDotSegment(p), isManagementEndpoint(p)
		if routerMatchesManagement(p) && !dotDot && !mgmt {
			t.Fatalf("%q reaches management endpoint %q but passes both checks", p, routerPath(p))
		}
		if dotDot {
			return // refused before the endpoint check; variations may drop the ".."
		}
		for _, v := range []string{
			asciiUpper(p),
			strings.ReplaceAll(p, "/", "//"),
			strings.ReplaceAll(p, "/", `\`),
			cleanPath(p),
		} {
			if got := isManagementEndpoint(v); got != mgmt {
				t.Fatalf("isManagementEndpoint(%q) = %v, but isManagementEndpoint(%q) = %v", p, mgmt, v, got)
			}
		}
	})
}

// routerPath resolves p the way a lenient router might, without path.Clean:
// "\" and "/" separate segments, empty and "." segments are dropped, ".."
// removes the previous segment, and ASCII letters are lowercased.
func routerPath(p string) string {
	var segs []string
	for seg := range strings.FieldsFuncSeq(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		switch seg {
		case ".":
		case "..":
			if len(segs) > 0 {
				segs = segs[:len(segs)-1]
			}
		default:
			segs = append(segs, seg)
		}
	}
	return asciiLower("/" + strings.Join(segs, "/"))
}

// routerMatchesManagement reports whether routerPath(p) is a management
// endpoint or below one.
func routerMatchesManagement(p string) bool {
	rp := routerPath(p)
	for _, e := range managementEndpoints {
		if rp == e || strings.HasPrefix(rp, e+"/") {
			return true
		}
	}
	return false
}

// asciiUpper and asciiLower change the case of ASCII letters only, as HTTP
// routers that match case-insensitively do.
func asciiUpper(s string) string {
	return strings.Map(func(r rune) rune {
		if 'a' <= r && r <= 'z' {
			return r - 'a' + 'A'
		}
		return r
	}, s)
}

func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if 'A' <= r && r <= 'Z' {
			return r - 'A' + 'a'
		}
		return r
	}, s)
}

// FuzzRequestTarget checks that a request target hasPathTarget accepts
// reaches the upstream with the path the proxy checked. The target is parsed
// as net/http's server parses a request line, rewritten by the proxy's own
// Rewrite, and the outgoing request target parsed again as a Go upstream
// (Ollama) would. The path fuzzer starts from a decoded path, so it can't see
// targets whose URL fields disagree, such as "a:x:/api/delete" (empty Path,
// forwarded as "x:/api/delete").
func FuzzRequestTarget(f *testing.F) {
	for _, s := range []string{
		"/v1/chat/completions", "/api/pull?x=1", "//api/pull", "/api/blobs%2F..", "/a%3Fb", "/a?",
		"a:x:/api/delete", "x:/api/delete", "http:%2F%2Fh/api/delete", "*", "http://localhost",
		"http://localhost/api/pull", "http://u:p@localhost/v1/models?q=1", "HTTP://H//api//pull",
	} {
		f.Add(s)
	}
	rp := newReverseProxy(&url.URL{Scheme: "http", Host: "localhost:1234"})
	f.Fuzz(func(t *testing.T, raw string) {
		in, err := url.ParseRequestURI(raw)
		if err != nil || !hasPathTarget(in) {
			return
		}
		req := &http.Request{Method: "GET", URL: in, Header: http.Header{}, Host: "localhost"}
		pr := &httputil.ProxyRequest{In: req, Out: req.Clone(context.Background())}
		rp.Rewrite(pr)
		if pr.Out.URL.User != nil {
			t.Fatalf("%q: user info kept on the outgoing URL", raw)
		}
		sent := pr.Out.URL.RequestURI()
		up, err := url.ParseRequestURI(sent)
		if err != nil {
			t.Fatalf("%q is forwarded as %q, which an upstream can't parse: %v", raw, sent, err)
		}
		if up.Path != in.Path {
			t.Fatalf("%q: proxy checked path %q, upstream sees %q (forwarded as %q)", raw, in.Path, up.Path, sent)
		}
	})
}
