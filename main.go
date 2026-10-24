package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v4"
)

// Config holds the proxy configuration
type Config struct {
	ProxyPort    int          `yaml:"proxy_port"`
	Server       ServerConfig `yaml:"server"`
	APIEndpoints []string     `yaml:"api_endpoints"`
	// ForwardUnmatched forwards paths outside APIEndpoints without metering
	// them instead of answering 404. Unset means true (forwardsUnmatched).
	ForwardUnmatched *bool `yaml:"forward_unmatched"`
	// ForwardManagement forwards requests that manage the upstream instead of
	// querying it (managementEndpoints) and protocol upgrades such as
	// WebSocket. Off by default: they are answered with 403 (refuseManagement).
	ForwardManagement bool          `yaml:"forward_management"`
	Metrics           MetricsConfig `yaml:"metrics"`
	Logging           LoggingConfig `yaml:"logging"`
}

// forwardsUnmatched reports whether paths outside APIEndpoints are forwarded.
func (c Config) forwardsUnmatched() bool {
	return c.ForwardUnmatched == nil || *c.ForwardUnmatched
}

type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

type MetricsConfig struct {
	HistoryPoints int `yaml:"history_points"`
}

type LoggingConfig struct {
	Debug bool `yaml:"debug"`
}

// Metrics stores collected metrics
type Metrics struct {
	TokensPerSecond float64 // most recent query
	TotalTokens     int     // cumulative over all queries
	QueryCount      int
	ModelName       string // most recent query that reported a model
	AvgTimePerQuery time.Duration
	History         []MetricsHistoryPoint // oldest first
	mu              sync.RWMutex
}

type MetricsHistoryPoint struct {
	Timestamp    time.Time
	TokensPerSec float64
	TotalTokens  int
	Duration     time.Duration
}

// metricsSnapshot is a consistent copy of the aggregate metrics taken while
// recording a query, so it can be displayed without holding metrics.mu.
type metricsSnapshot struct {
	QueryCount      int
	TotalTokens     int
	AvgTimePerQuery time.Duration
	AvgTokensPerSec float64 // over History, ignoring queries without a rate
}

// MetricsData holds the metrics of a single proxied query
type MetricsData struct {
	ModelName        string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	TokensPerSecond  float64
	TPSEstimated     bool          // TokensPerSecond derived by completeDerived, not reported
	TimeToFirstToken time.Duration // as reported by the upstream; 0 if unknown
	Duration         time.Duration
}

// maxCaptureBytes bounds how much of a response body (or of a single SSE
// line) is buffered for metrics parsing. Larger bodies are proxied as usual
// but not inspected.
const maxCaptureBytes = 16 << 20

// shutdownTimeout bounds how long a graceful shutdown waits for in-flight
// requests; below systemd's default 90 s stop timeout.
const shutdownTimeout = 60 * time.Second

// Global config and metrics
var (
	cfg     Config
	proxy   *httputil.ReverseProxy
	metrics Metrics
	out     io.Writer    = os.Stdout // destination for per-request status lines
	csvLog  *csvRecorder             // per-query CSV output (--csv flag); nil when disabled
)

func main() {
	opts, err := parseFlags(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(0)
	}
	if err != nil {
		os.Exit(2) // parseFlags has printed the error and usage
	}

	fmt.Println("=== LLM Metrics Proxy ===")

	// Load configuration
	if err := loadConfig(opts.configPath); err != nil {
		fmt.Printf("Error loading config: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Loaded config from %s\n", opts.configPath)
	debugf("config: proxy_port=%d upstream=%s:%d api_endpoints=%q forward_unmatched=%t forward_management=%t history_points=%d",
		cfg.ProxyPort, cfg.Server.Host, cfg.Server.Port, cfg.APIEndpoints, cfg.forwardsUnmatched(), cfg.ForwardManagement, cfg.Metrics.HistoryPoints)

	if opts.csvPath != "" {
		rec, err := openCSVLog(opts.csvPath)
		if err != nil {
			fmt.Printf("Error opening CSV file: %v\n", err)
			os.Exit(1)
		}
		csvLog = rec
		defer rec.Close()
		fmt.Printf("Writing statistics to %s\n", opts.csvPath)
	}

	target := upstreamURL()
	proxy = newReverseProxy(target)

	// Setup signal handling for graceful shutdown: stop is done on the first
	// signal, force on the second.
	stop, stopNow := context.WithCancel(context.Background())
	defer stopNow()
	force, forceNow := context.WithCancel(context.Background())
	defer forceNow()
	setupSignalHandling(stopNow, forceNow)

	// Start proxy server
	fmt.Printf("Proxy listening on port %d\n", cfg.ProxyPort)
	if backend := detectBackend(target); backend != "" {
		fmt.Printf("Forwarding to %s at %s\n", backend, target.Host)
	} else {
		fmt.Printf("Forwarding to upstream at %s (server type not detected)\n", target.Host)
	}

	if err := runProxy(stop, force); err != nil {
		fmt.Printf("Error running proxy: %v\n", err)
		os.Exit(1)
	}
}

// flagUsage prints the flags in the GNU style (--name). The flag package
// accepts both -name and --name, but its default help shows -name.
func flagUsage(fs *flag.FlagSet) {
	w := fs.Output()
	fmt.Fprintf(w, "Usage: %s [options]\n\nOptions:\n", filepath.Base(os.Args[0]))
	fs.VisitAll(func(f *flag.Flag) {
		arg, usage := flag.UnquoteUsage(f)
		if f.DefValue != "" {
			usage += fmt.Sprintf(" (default %q)", f.DefValue)
		}
		fmt.Fprintf(w, "  --%s %s\n    \t%s\n", f.Name, arg, usage)
	})
}

// options holds the command-line flags.
type options struct {
	configPath string
	csvPath    string
}

// parseFlags parses the command-line arguments (without the program name).
// Errors, and the usage for --help, are printed to output; --help returns
// flag.ErrHelp. Relative paths are resolved against the working directory.
func parseFlags(args []string, output io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("metrics_proxy", flag.ContinueOnError)
	fs.SetOutput(output)
	fs.Usage = func() { flagUsage(fs) }
	fs.StringVar(&o.configPath, "config", "config.yaml", "read the configuration from `file`")
	fs.StringVar(&o.csvPath, "csv", "", "append per-query statistics to this CSV `file`")

	if err := fs.Parse(args); err != nil {
		return o, err
	}
	// Positional arguments are not used; reject them rather than silently
	// ignoring e.g. "metrics_proxy my.yaml".
	if fs.NArg() > 0 {
		err := fmt.Errorf("unexpected argument %q", fs.Arg(0))
		fmt.Fprintln(output, err)
		fs.Usage()
		return o, err
	}
	if o.configPath == "" {
		err := errors.New("--config: file path must not be empty")
		fmt.Fprintln(output, err)
		return o, err
	}
	return o, nil
}

// loadConfig reads and parses the configuration file at path into cfg.
func loadConfig(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read config file: %w", err)
	}

	c, err := parseConfig(data)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	cfg = c
	return nil
}

// parseConfig unmarshals YAML configuration and fills in defaults for unset
// fields. Unknown keys (typos, removed settings), duplicate keys and a second
// YAML document are errors.
func parseConfig(data []byte) (Config, error) {
	c, err := decodeConfig(data)
	if err != nil {
		return c, fmt.Errorf("failed to parse config file: %w", err)
	}

	if c.ProxyPort == 0 {
		c.ProxyPort = 11436 // clear of 8080 (llama.cpp and many others) and Ollama's 11434/11435
	}
	if c.Server.Host == "" {
		c.Server.Host = "localhost"
	}
	if c.Server.Port == 0 {
		c.Server.Port = 1234
	}
	if len(c.APIEndpoints) == 0 {
		c.APIEndpoints = []string{"/api/v1/*", "/v1/*"} // LM Studio native + OpenAI-compatible
	}
	if c.Metrics.HistoryPoints <= 0 {
		c.Metrics.HistoryPoints = 10
	}
	return c, nil
}

// decodeConfig decodes the YAML document in data; an empty file gives the zero
// Config. Settings after a "---" separator are rejected rather than ignored.
func decodeConfig(data []byte) (Config, error) {
	var c Config
	l, err := yaml.NewLoader(bytes.NewReader(data),
		yaml.WithKnownFields(), // reject misspelled and unsupported keys
		yaml.WithUniqueKeys(),  // reject duplicate keys (the v4 default, pinned here)
	)
	if err != nil {
		return c, err
	}
	if err := l.Load(&c); err != nil && !errors.Is(err, io.EOF) { // EOF: empty file
		return c, err
	}
	var next yaml.Node
	if err := l.Load(&next); !errors.Is(err, io.EOF) {
		if err != nil {
			return c, err // e.g. text after a "..." document end marker
		}
		return c, fmt.Errorf("line %d: a second YAML document starts here; a config file holds one", next.Line)
	}
	return c, nil
}

// upstreamURL returns the base URL of the configured upstream server.
func upstreamURL() *url.URL {
	return &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(cfg.Server.Host, strconv.Itoa(cfg.Server.Port)),
	}
}

// detectBackend identifies the upstream server type for the startup banner.
// It checks response bodies rather than status codes, since LM Studio answers
// unknown endpoints with 200 and an error body. It returns "" if the upstream
// is unreachable or not recognized.
func detectBackend(target *url.URL) string {
	client := &http.Client{Timeout: 2 * time.Second}
	get := func(path string) (map[string]json.RawMessage, error) {
		resp, err := client.Get(target.JoinPath(path).String())
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		var body map[string]json.RawMessage
		if resp.StatusCode != http.StatusOK {
			debugf("backend probe %s: status %d", path, resp.StatusCode)
			return nil, nil
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
			debugf("backend probe %s: body is not a JSON object: %v", path, err)
			return nil, nil
		}
		return body, nil
	}

	probes := []struct{ name, path, key string }{
		{"Ollama", "/api/version", "version"},
		{"LM Studio", "/api/v1/models", "models"},
		{"OpenAI-compatible server", "/v1/models", "data"},
	}
	for _, p := range probes {
		body, err := get(p.path)
		if err != nil {
			debugf("backend probe %s: %v", p.path, err)
			return "" // unreachable; don't wait for the other probes
		}
		if _, ok := body[p.key]; ok {
			debugf("backend probe %s: found %q, upstream is %s", p.path, p.key, p.name)
			return p.name
		}
		if body != nil {
			debugf("backend probe %s: no %q key, not %s", p.path, p.key, p.name)
		}
	}
	return ""
}

// setupSignalHandling returns a context that is done on the first SIGINT or
// SIGTERM (start a graceful shutdown) and one that is done on the second
// (abort it).
// setupSignalHandling calls onFirst on the first SIGINT or SIGTERM (start a
// graceful shutdown) and onSecond on the second (abort it).
func setupSignalHandling(onFirst, onSecond func()) {
	sigChan := make(chan os.Signal, 2)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		onFirst()
		<-sigChan
		onSecond()
	}()
}

func runProxy(stop, force context.Context) error {
	server := &http.Server{
		Handler: http.HandlerFunc(handleRequest),
		// No ReadTimeout/WriteTimeout: generations can take minutes, and an
		// expiring ReadTimeout cancels the request context mid-response.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.ProxyPort))
	if err != nil {
		return err
	}

	fmt.Println("Press Ctrl+C to stop")
	return serve(stop, force, server, ln, shutdownTimeout)
}

// serve runs server on ln until stop is done, then shuts it down gracefully:
// it stops accepting connections and waits up to timeout for in-flight
// requests (streamed generations included) to finish. If the timeout passes
// or force is done first, the remaining connections are closed and their
// queries are recorded as incomplete.
func serve(stop, force context.Context, server *http.Server, ln net.Listener, timeout time.Duration) error {
	var inFlight atomic.Int64
	next := server.Handler
	server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inFlight.Add(1)
		defer inFlight.Add(-1)
		next.ServeHTTP(w, r)
	})

	errc := make(chan error, 1)
	go func() { errc <- server.Serve(ln) }()

	select {
	case err := <-errc:
		return err
	case <-stop.Done():
	}

	if n := inFlight.Load(); n > 0 {
		fmt.Fprintf(out, "\nShutting down: waiting up to %s for %d in-flight request(s) (Ctrl+C again to force)\n", timeout, n)
	} else {
		fmt.Fprintln(out, "\nShutting down")
	}
	ctx, cancel := context.WithTimeout(force, timeout)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		reason := "timed out"
		if force.Err() != nil {
			reason = "forced"
		}
		fmt.Fprintf(out, "Shutdown %s: closing %d request(s)\n", reason, inFlight.Load())
		server.Close()
		// Give the aborted requests a moment to record their metrics.
		for deadline := time.Now().Add(2 * time.Second); inFlight.Load() > 0 && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
	}
	<-errc //nolint:errcheck // always http.ErrServerClosed after Shutdown/Close
	fmt.Fprintln(out, "Proxy stopped")
	return nil
}

// isAPIEndpoint checks if the request path matches an API endpoint pattern.
// A trailing "*" in a pattern is ignored; patterns match as path prefixes.
func isAPIEndpoint(path string) bool {
	for _, pattern := range cfg.APIEndpoints {
		if strings.HasPrefix(path, strings.TrimSuffix(pattern, "*")) {
			return true
		}
	}
	return false
}

// managementEndpoints are upstream API paths that change which models the
// server has or runs, or concern its ollama.com account, rather than query
// it. Unless forward_management is set they are not forwarded, even when they
// match api_endpoints (such as Ollama's "/api/*"). Queries that use the
// account (cloud models, Ollama's web search and fetch) are forwarded like
// any other query.
var managementEndpoints = []string{
	// Ollama native API: model management, then the signed-in account
	"/api/pull", "/api/push", "/api/create", "/api/copy", "/api/delete", "/api/blobs",
	"/api/me", "/api/signout", "/api/user/keys",
	// LM Studio native REST API
	"/api/v1/models/load", "/api/v1/models/unload", "/api/v1/models/download",
}

// isManagementEndpoint reports whether the (decoded) request path is one of
// managementEndpoints or below one. The path is cleaned and lowercased first,
// so dot segments, repeated or trailing slashes and case variants that an
// upstream router may accept match too.
func isManagementEndpoint(p string) bool {
	p = path.Clean("/" + strings.ToLower(p))
	for _, e := range managementEndpoints {
		if p == e || strings.HasPrefix(p, e+"/") {
			return true
		}
	}
	return false
}

// offersUpgrade reports whether h's Upgrade header offers a protocol other
// than h2c, i.e. asks to turn the connection into a tunnel (WebSocket).
func offersUpgrade(h http.Header) bool {
	for _, v := range h.Values("Upgrade") {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" && !strings.EqualFold(p, "h2c") {
				return true
			}
		}
	}
	return false
}

// refuseManagement answers r with 403 Forbidden, without contacting the
// upstream, if it is a management request or asks for a protocol upgrade, and
// reports whether it did. It runs while forward_management is false.
//
// An upgraded connection is a tunnel to the upstream: requests sent over it
// don't pass through handleRequest. LM Studio's SDK API runs over WebSocket
// (/llm, /repository, ...) and loads and downloads models that way. An
// Upgrade header offering only h2c is removed instead: clients such as Java's
// HttpClient add it to ordinary requests and stay on HTTP/1.1 when the
// server doesn't switch, so the request is forwarded as plain HTTP/1.1.
func refuseManagement(w http.ResponseWriter, r *http.Request) bool {
	var what string
	switch {
	case isManagementEndpoint(r.URL.Path):
		what = "Management request"
	case offersUpgrade(r.Header):
		what = "Protocol upgrade"
	default:
		r.Header.Del("Upgrade") // at most an h2c offer; the upstream can't switch without it
		return false
	}
	fmt.Fprintf(out, "[Proxy] %s not forwarded: %s %s (forward_management: false)\n", what, r.Method, escapeControls(r.URL.Path))
	http.Error(w, what+" not forwarded by the proxy (forward_management: false)", http.StatusForbidden)
	return true
}

// requestInfo describes an incoming request; it travels to the response
// side of the proxy via the request context.
type requestInfo struct {
	id     uint64 // sequence number, tells concurrent requests apart in debug lines
	start  time.Time
	method string
	path   string
	// metered is false for paths outside api_endpoints (forward_unmatched).
	metered bool
}

// requestSeq numbers forwarded requests for debug output.
var requestSeq atomic.Uint64

// debugf prints a [DEBUG] status line when logging.debug is enabled.
func debugf(format string, args ...any) {
	if cfg.Logging.Debug {
		fmt.Fprintf(out, "[DEBUG] "+format+"\n", args...)
	}
}

// escapeControls makes text from clients or the upstream (paths, URLs, model
// names, errors) safe to print to the terminal or log, so it can't send escape
// sequences or start fake lines. Control characters (C0, DEL, C1), the Unicode
// line and paragraph separators and the bidirectional text controls (which
// can reorder how the rest of a line is displayed) are written as Go escapes
// (\n, \x1b, \u009b, \u2028, \u202e), and bytes that aren't valid UTF-8 as
// \xNN. Everything else is kept as is: backslashes, printable non-ASCII text
// and other invisible format characters such as the zero-width joiner in
// emoji. Text with nothing to escape is returned without copying.
func escapeControls(s string) string {
	var b []byte // nil until something needs escaping
	done := 0    // s[:done] has been written to b
	for i := 0; i < len(s); {
		if c := s[i]; c >= ' ' && c < 0x7f {
			i++ // printable ASCII, the common case
			continue
		}
		n, escape := escapeAt(s, i)
		if escape {
			if b == nil {
				b = make([]byte, 0, len(s)+16)
			}
			b = appendGoEscape(append(b, s[done:i]...), s[i:i+n])
			done = i + n
		}
		i += n
	}
	if b == nil {
		return s
	}
	return string(append(b, s[done:]...))
}

// escapeAt returns the length of the rune (or invalid UTF-8 byte) at s[i] and
// whether escapeControls escapes it.
func escapeAt(s string, i int) (int, bool) {
	if c := s[i]; c < utf8.RuneSelf {
		return 1, c < ' ' || c == 0x7f
	}
	r, n := utf8.DecodeRuneInString(s[i:])
	if r == utf8.RuneError && n == 1 {
		return 1, true
	}
	return n, unicode.IsControl(r) || unicode.In(r, unicode.Bidi_Control, unicode.Zl, unicode.Zp)
}

// appendGoEscape appends the Go escape of c, a single rune or invalid UTF-8
// byte, as strconv.QuoteToASCII writes it but without the quotes.
func appendGoEscape(b []byte, c string) []byte {
	start := len(b)
	b = strconv.AppendQuoteToASCII(b, c)
	return append(b[:start], b[start+1:len(b)-1]...)
}

type requestInfoKey struct{}

// handleRequest processes incoming requests
func handleRequest(w http.ResponseWriter, r *http.Request) {
	if !cfg.ForwardManagement && refuseManagement(w, r) {
		return
	}
	metered := isAPIEndpoint(r.URL.Path)
	if !metered && !cfg.forwardsUnmatched() {
		fmt.Fprintf(out, "[Proxy] Non-API request: %s %s\n", r.Method, escapeControls(r.URL.Path))
		http.NotFound(w, r)
		return
	}

	info := &requestInfo{id: requestSeq.Add(1), start: time.Now(), method: r.Method, path: r.URL.Path, metered: metered}
	// debugf's arguments are evaluated even when it prints nothing; don't
	// escape a client's (possibly long) request URI unless it is printed.
	if cfg.Logging.Debug {
		debugf("req %d: %s %s from %s (metered: %t)",
			info.id, r.Method, escapeControls(r.URL.RequestURI()), r.RemoteAddr, metered)
	}
	ctx := context.WithValue(r.Context(), requestInfoKey{}, info)
	proxy.ServeHTTP(w, r.WithContext(ctx))
}

// newReverseProxy returns a reverse proxy to target that records metrics
// from response bodies while streaming them to the client.
func newReverseProxy(target *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.SetXForwarded()
			// Let the transport negotiate compression and decompress
			// transparently, so the body can be inspected for metrics.
			pr.Out.Header.Del("Accept-Encoding")
		},
		// Flush every write so streamed tokens reach the client immediately.
		FlushInterval:  -1,
		ModifyResponse: meterResponse,
		ErrorHandler:   proxyError,
	}
}

// meterResponse wraps the upstream response body so metrics are recorded
// once the body has been streamed to the client.
func meterResponse(resp *http.Response) error {
	info, ok := resp.Request.Context().Value(requestInfoKey{}).(*requestInfo)
	if !ok {
		return nil
	}
	contentType := resp.Header.Get("Content-Type")
	if cfg.Logging.Debug { // only escape the URL when it is printed (see handleRequest)
		debugf("req %d: upstream %s -> %d after %.2fs, Content-Type %q",
			info.id, escapeControls(resp.Request.URL.String()), resp.StatusCode, time.Since(info.start).Seconds(), contentType)
	}
	if !info.metered {
		// Logged when the headers arrive; the body is passed through as is.
		fmt.Fprintf(out, "[Proxy] %s %s %d (%.2fs, not metered)\n",
			info.method, escapeControls(info.path), resp.StatusCode, time.Since(info.start).Seconds())
		return nil
	}
	// A 101 response body is used for the upgraded connection and must not be wrapped.
	if resp.StatusCode == http.StatusSwitchingProtocols {
		debugf("req %d: protocol upgrade, connection not inspected", info.id)
		return nil
	}

	c := newCapture(contentType)
	if c == nil {
		debugf("req %d: Content-Type not inspected; only the duration is recorded", info.id)
	}
	resp.Body = &meteredBody{
		ReadCloser: resp.Body,
		info:       info,
		status:     resp.StatusCode,
		capture:    c,
	}
	return nil
}

// proxyError reports upstream failures to the client as 502 Bad Gateway.
func proxyError(w http.ResponseWriter, r *http.Request, err error) {
	fmt.Fprintf(out, "[ERROR] %s %s: %s\n", r.Method, escapeControls(r.URL.Path), escapeControls(err.Error()))
	http.Error(w, "Failed to connect to the upstream LLM server", http.StatusBadGateway)
}

// meteredBody passes an upstream response body through to the reverse proxy,
// feeding a copy to a capture, and records metrics when the body ends.
type meteredBody struct {
	io.ReadCloser
	info      *requestInfo
	status    int
	capture   capture // nil when the body is not inspected
	firstByte time.Time
	bytes     int64
	eof       bool
	once      sync.Once
}

func (b *meteredBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.bytes += int64(n)
		if b.firstByte.IsZero() {
			b.firstByte = time.Now()
		}
		if b.capture != nil {
			_, _ = b.capture.Write(p[:n]) // captures never fail; parse problems show in diag()
		}
	}
	if err == io.EOF {
		b.eof = true
		b.finish()
	}
	return n, err
}

// Close records metrics even if the body was not read to the end, e.g.
// because the client disconnected mid-stream.
func (b *meteredBody) Close() error {
	err := b.ReadCloser.Close()
	b.finish()
	return err
}

func (b *meteredBody) finish() {
	b.once.Do(func() {
		end := time.Now()

		var data MetricsData
		var streamTime time.Duration
		if b.capture != nil {
			data = b.capture.finish()
		}
		switch b.capture.(type) {
		case *sseCapture, *ndjsonCapture:
			if !b.firstByte.IsZero() {
				streamTime = end.Sub(b.firstByte)
			}
		}
		data.Duration = end.Sub(b.info.start)
		basis := data.completeDerived(streamTime)

		if cfg.Logging.Debug {
			b.debugSummary(data, streamTime, basis)
		}
		recordQuery(b.info, b.status, data, b.eof)
	})
}

// debugSummary explains how the metrics of a finished body were obtained.
func (b *meteredBody) debugSummary(data MetricsData, streamTime time.Duration, basis string) {
	id := b.info.id
	ended := "at EOF"
	if !b.eof {
		ended = "before EOF (client disconnected or shutdown)"
	}
	if streamTime > 0 {
		ended += fmt.Sprintf(", stream time %.2fs", streamTime.Seconds())
	}
	debugf("req %d: body ended %s after %d bytes", id, ended, b.bytes)
	if b.capture == nil {
		return
	}

	d := b.capture.diag()
	debugf("req %d: parsed as %s: %d JSON document(s), %d invalid, %d skipped as larger than %d MiB",
		id, d.name, d.documents, d.invalid, d.oversized, maxCaptureBytes>>20)
	if basis != "" {
		debugf("req %d: no tokens/s reported; estimated as %s", id, basis)
	}
	if !data.hasInference() {
		debugf("req %d: no model, token or rate fields found; not counted as a query", id)
	} else if d.name == "SSE" && data.TotalTokens == 0 {
		debugf(`req %d: stream has no token usage; OpenAI-compatible clients must send "stream_options": {"include_usage": true}`, id)
	}
}

// recordQuery updates the global metrics and prints a status line. Responses
// without inference data (model listings, errors, ...) are only logged.
func recordQuery(info *requestInfo, status int, data MetricsData, complete bool) {
	if !data.hasInference() {
		fmt.Fprintf(out, "[Proxy] %s %s %d (%.2fs)\n",
			info.method, escapeControls(info.path), status, data.Duration.Seconds())
		return
	}

	now := time.Now()
	snap := updateGlobalMetrics(data)
	displayMetrics(now, info, status, data, snap, complete)
	if csvLog != nil {
		if err := csvLog.record(now, info, status, data, complete); err != nil {
			fmt.Fprintf(out, "[ERROR] writing CSV: %v\n", err)
		}
	}
}

// capture inspects a response body as it is streamed to the client.
type capture interface {
	io.Writer
	// finish returns the metrics found in everything written so far.
	finish() MetricsData
	// diag reports what was parsed, for debug output.
	diag() captureDiag
}

// captureDiag counts the JSON documents (whole body, SSE events or NDJSON
// lines) a capture has seen.
type captureDiag struct {
	name      string // "JSON", "SSE" or "NDJSON"
	documents int    // parsed as JSON objects
	invalid   int    // not valid JSON objects
	oversized int    // bodies, lines or events skipped for exceeding maxCaptureBytes
}

// count records the outcome of parsing one document.
func (d *captureDiag) count(ok bool) {
	if ok {
		d.documents++
	} else {
		d.invalid++
	}
}

// newCapture returns a capture for bodies of the given Content-Type, or nil
// if bodies of that type carry no metrics.
func newCapture(contentType string) capture {
	mediaType, _, _ := mime.ParseMediaType(contentType)
	switch {
	case mediaType == "text/event-stream":
		return &sseCapture{}
	case mediaType == "application/x-ndjson":
		return &ndjsonCapture{}
	case mediaType == "application/json" || strings.HasSuffix(mediaType, "+json"):
		return &jsonCapture{}
	}
	return nil
}

// jsonCapture buffers a JSON body and parses it once it is complete.
type jsonCapture struct {
	buf      bytes.Buffer
	overflow bool
	d        captureDiag
}

func (c *jsonCapture) Write(p []byte) (int, error) {
	if c.overflow {
		return len(p), nil
	}
	if c.buf.Len()+len(p) > maxCaptureBytes {
		c.overflow = true
		c.buf = bytes.Buffer{}
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *jsonCapture) finish() MetricsData {
	var d MetricsData
	c.d = captureDiag{name: "JSON"}
	switch {
	case c.overflow:
		c.d.oversized = 1
	case c.buf.Len() > 0:
		c.d.count(d.mergeJSON(c.buf.Bytes()))
	}
	return d
}

func (c *jsonCapture) diag() captureDiag { return c.d }

// lineSplitter splits a body written in arbitrary chunks into lines and
// passes each line, without its "\n" or "\r\n" terminator, to emit. Lines
// longer than maxCaptureBytes are discarded.
type lineSplitter struct {
	line      []byte // incomplete line carried over between writes
	skipLine  bool   // current line exceeds maxCaptureBytes and is discarded
	oversized int    // number of lines discarded
}

func (s *lineSplitter) write(p []byte, emit func(line []byte)) {
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.appendLine(p)
			return
		}
		s.appendLine(p[:i])
		s.endLine(emit)
		p = p[i+1:]
	}
}

// flush emits an unterminated last line, if any, at the end of the body.
func (s *lineSplitter) flush(emit func(line []byte)) {
	if len(s.line) > 0 {
		s.endLine(emit)
	}
}

// endLine emits the current line (which may be empty) and starts a new one.
func (s *lineSplitter) endLine(emit func(line []byte)) {
	if !s.skipLine {
		emit(bytes.TrimSuffix(s.line, []byte("\r")))
	}
	s.line, s.skipLine = s.line[:0], false
}

func (s *lineSplitter) appendLine(p []byte) {
	if s.skipLine {
		return
	}
	if len(s.line)+len(p) > maxCaptureBytes {
		s.line, s.skipLine = s.line[:0], true
		s.oversized++
		return
	}
	s.line = append(s.line, p...)
}

// sseCapture parses a text/event-stream body event by event, so memory use
// is bounded by the largest event rather than the whole stream. Metrics from
// later events override earlier ones.
type sseCapture struct {
	lines   lineSplitter
	data    []byte // data field of the event being assembled
	metrics MetricsData
	d       captureDiag
}

func (c *sseCapture) Write(p []byte) (int, error) {
	c.lines.write(p, c.processLine)
	return len(p), nil
}

func (c *sseCapture) processLine(line []byte) {
	switch {
	case len(line) == 0:
		c.dispatch()
	case bytes.HasPrefix(line, []byte("data:")):
		value := bytes.TrimPrefix(line[len("data:"):], []byte(" "))
		if len(c.data)+len(value) > maxCaptureBytes {
			c.data = c.data[:0]
			c.d.oversized++
			return
		}
		if len(c.data) > 0 {
			c.data = append(c.data, '\n')
		}
		c.data = append(c.data, value...)
	}
	// Other fields ("event:", "id:", "retry:") and comments are not needed:
	// payloads are recognized by their content.
}

func (c *sseCapture) dispatch() {
	if len(c.data) > 0 && !bytes.Equal(c.data, []byte("[DONE]")) {
		c.d.count(c.metrics.mergeJSON(c.data))
	}
	c.data = c.data[:0]
}

func (c *sseCapture) finish() MetricsData {
	c.lines.flush(c.processLine)
	c.dispatch()
	return c.metrics
}

func (c *sseCapture) diag() captureDiag {
	d := c.d
	d.name = "SSE"
	d.oversized += c.lines.oversized
	return d
}

// ndjsonCapture parses a newline-delimited JSON body (Ollama's native
// streaming format) line by line. Metrics from later lines override earlier
// ones; Ollama reports its stats in the final "done" line.
type ndjsonCapture struct {
	lines   lineSplitter
	metrics MetricsData
	d       captureDiag
}

func (c *ndjsonCapture) Write(p []byte) (int, error) {
	c.lines.write(p, c.processLine)
	return len(p), nil
}

func (c *ndjsonCapture) processLine(line []byte) {
	if len(bytes.TrimSpace(line)) > 0 {
		c.d.count(c.metrics.mergeJSON(line))
	}
}

func (c *ndjsonCapture) finish() MetricsData {
	c.lines.flush(c.processLine)
	return c.metrics
}

func (c *ndjsonCapture) diag() captureDiag {
	d := c.d
	d.name = "NDJSON"
	d.oversized += c.lines.oversized
	return d
}

// llmPayload is the subset of upstream response fields used for metrics. It
// covers OpenAI-compatible responses (/v1/*), LM Studio's native REST API
// v0 (/api/v0/*) and v1 (/api/v1/*), and Ollama's native API (/api/*), as
// JSON bodies, SSE events and NDJSON lines.
type llmPayload struct {
	Model           string `json:"model"`             // OpenAI-compatible, API v0
	ModelInstanceID string `json:"model_instance_id"` // API v1
	Usage           *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		InputTokens      int `json:"input_tokens"`  // OpenAI Responses API
		OutputTokens     int `json:"output_tokens"` // OpenAI Responses API
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Stats *struct {
		TokensPerSecond         float64 `json:"tokens_per_second"`
		TimeToFirstToken        float64 `json:"time_to_first_token"`         // API v0
		TimeToFirstTokenSeconds float64 `json:"time_to_first_token_seconds"` // API v1
		InputTokens             int     `json:"input_tokens"`                // API v1
		TotalOutputTokens       int     `json:"total_output_tokens"`         // API v1
	} `json:"stats"`
	Timings *struct {
		PredictedPerSecond float64 `json:"predicted_per_second"`
	} `json:"timings"` // Ollama OpenAI-compatible
	PromptEvalCount int   `json:"prompt_eval_count"` // Ollama native
	EvalCount       int   `json:"eval_count"`        // Ollama native
	EvalDuration    int64 `json:"eval_duration"`     // Ollama native, nanoseconds
	// Wrapped payloads: the API v1 "chat.end" SSE event carries the response
	// in "result", OpenAI Responses API events in "response". Kept raw so an
	// unrelated field of the same name doesn't fail the whole parse.
	Result   json.RawMessage `json:"result"`
	Response json.RawMessage `json:"response"`
}

// mergeJSON applies the metrics found in a JSON document to d and reports
// whether it was a JSON object. Other documents are ignored; a field of
// unexpected type is skipped without discarding the others.
func (d *MetricsData) mergeJSON(raw []byte) bool {
	var p llmPayload
	var typeErr *json.UnmarshalTypeError
	err := json.Unmarshal(raw, &p)
	// A type error without a field is the document itself (array, string, ...).
	if err != nil && (!errors.As(err, &typeErr) || typeErr.Field == "") {
		return false
	}
	d.merge(&p)
	return true
}

// merge applies the metrics in p, including wrapped payloads, to d. Fields
// p reports override those already in d; fields it lacks are left as is.
func (d *MetricsData) merge(p *llmPayload) {
	if p.Model != "" {
		d.ModelName = p.Model
	}
	if p.ModelInstanceID != "" {
		d.ModelName = p.ModelInstanceID
	}
	if u := p.Usage; u != nil {
		setFirstPositive(&d.PromptTokens, u.PromptTokens, u.InputTokens)
		setFirstPositive(&d.CompletionTokens, u.CompletionTokens, u.OutputTokens)
		setFirstPositive(&d.TotalTokens, u.TotalTokens)
	}
	if s := p.Stats; s != nil {
		if s.TokensPerSecond > 0 {
			d.TokensPerSecond = s.TokensPerSecond
		}
		if ttft := max(s.TimeToFirstToken, s.TimeToFirstTokenSeconds); ttft > 0 {
			d.TimeToFirstToken = time.Duration(ttft * float64(time.Second))
		}
		setFirstPositive(&d.PromptTokens, s.InputTokens)
		setFirstPositive(&d.CompletionTokens, s.TotalOutputTokens)
	}
	if t := p.Timings; t != nil && t.PredictedPerSecond > 0 {
		d.TokensPerSecond = t.PredictedPerSecond
	}
	setFirstPositive(&d.PromptTokens, p.PromptEvalCount)
	setFirstPositive(&d.CompletionTokens, p.EvalCount)
	if p.EvalCount > 0 && p.EvalDuration > 0 {
		d.TokensPerSecond = float64(p.EvalCount) / time.Duration(p.EvalDuration).Seconds()
	}
	for _, raw := range []json.RawMessage{p.Result, p.Response} {
		if len(raw) > 0 {
			d.mergeJSON(raw)
		}
	}
}

// setFirstPositive sets *dst to the first positive value in vals, leaving it
// unchanged if there is none.
func setFirstPositive(dst *int, vals ...int) {
	for _, v := range vals {
		if v > 0 {
			*dst = v
			return
		}
	}
}

// completeDerived fills in totals and throughput the upstream did not report.
// streamTime is the time between the first and last body byte of a streamed
// response, or 0 if the response was not streamed. It returns how the rate
// was estimated, or "" if it was reported or could not be estimated.
func (d *MetricsData) completeDerived(streamTime time.Duration) (basis string) {
	if d.TotalTokens == 0 {
		d.TotalTokens = d.PromptTokens + d.CompletionTokens
	}
	if d.TokensPerSecond > 0 || d.CompletionTokens == 0 {
		return ""
	}

	// Estimate generation time; for non-streamed responses without a
	// reported time to first token this includes prompt processing.
	genTime := d.Duration
	basis = "completion tokens / request duration (includes prompt processing)"
	if d.TimeToFirstToken > 0 && d.TimeToFirstToken < genTime {
		genTime -= d.TimeToFirstToken
		basis = "completion tokens / (request duration - time to first token)"
	} else if streamTime > 0 {
		genTime = streamTime
		basis = "completion tokens / stream time (first to last body byte)"
	}
	if genTime <= 0 {
		return ""
	}
	d.TokensPerSecond = float64(d.CompletionTokens) / genTime.Seconds()
	d.TPSEstimated = true
	return basis
}

// hasInference reports whether d describes a model query rather than, for
// example, a model listing or an error response.
func (d MetricsData) hasInference() bool {
	return d.ModelName != "" || d.TotalTokens > 0 || d.TokensPerSecond > 0
}

// updateGlobalMetrics updates the global metrics store and returns a snapshot
// of the aggregates including this query.
func updateGlobalMetrics(data MetricsData) metricsSnapshot {
	metrics.mu.Lock()
	defer metrics.mu.Unlock()

	metrics.QueryCount++
	metrics.TotalTokens += data.TotalTokens
	metrics.TokensPerSecond = data.TokensPerSecond
	if data.ModelName != "" {
		metrics.ModelName = data.ModelName
	}

	// Running average of time per query
	metrics.AvgTimePerQuery += (data.Duration - metrics.AvgTimePerQuery) / time.Duration(metrics.QueryCount)

	// Add to history
	metrics.History = append(metrics.History, MetricsHistoryPoint{
		Timestamp:    time.Now(),
		TokensPerSec: data.TokensPerSecond,
		TotalTokens:  data.TotalTokens,
		Duration:     data.Duration,
	})

	// Keep only the last N points
	if len(metrics.History) > cfg.Metrics.HistoryPoints {
		metrics.History = metrics.History[len(metrics.History)-cfg.Metrics.HistoryPoints:]
	}

	var tpsSum float64
	var tpsCount int
	for _, h := range metrics.History {
		if h.TokensPerSec > 0 {
			tpsSum += h.TokensPerSec
			tpsCount++
		}
	}

	snap := metricsSnapshot{
		QueryCount:      metrics.QueryCount,
		TotalTokens:     metrics.TotalTokens,
		AvgTimePerQuery: metrics.AvgTimePerQuery,
	}
	if tpsCount > 0 {
		snap.AvgTokensPerSec = tpsSum / float64(tpsCount)
	}
	return snap
}

// displayMetrics shows the metrics of a query and the aggregates in the terminal
func displayMetrics(now time.Time, info *requestInfo, status int, data MetricsData, snap metricsSnapshot, complete bool) {
	fmt.Fprintln(out, formatMetricsLine(now, info, status, data, snap, complete))
}

// csvHeader names the columns written by csvRecorder.record. Unknown values
// (no tokens, TPS or TTFT reported) are left empty rather than written as 0.
var csvHeader = []string{
	"timestamp", "method", "path", "status", "model",
	"prompt_tokens", "completion_tokens", "total_tokens",
	"tokens_per_second", "tps_estimated", "ttft_seconds", "duration_seconds", "complete",
}

// csvRecorder appends one row per recorded query to a CSV file. Rows are
// flushed as they are written, so the file is usable while the proxy runs.
type csvRecorder struct {
	mu     sync.Mutex
	f      *os.File
	w      *csv.Writer
	closed bool
}

// openCSVLog opens path for appending, creating it if needed. The header is
// written only when the file is empty, so restarts keep adding to one file.
func openCSVLog(path string) (*csvRecorder, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	r := &csvRecorder{f: f, w: csv.NewWriter(f)}
	if st, err := f.Stat(); err != nil || st.Size() == 0 {
		_ = r.w.Write(csvHeader) // write errors are sticky; checked via r.w.Error()
		r.w.Flush()
		if err := r.w.Error(); err != nil {
			f.Close()
			return nil, err
		}
	}
	return r, nil
}

func (r *csvRecorder) record(now time.Time, info *requestInfo, status int, data MetricsData, complete bool) error {
	optInt := func(n int) string {
		if n == 0 {
			return ""
		}
		return strconv.Itoa(n)
	}
	tps, ttft := "", ""
	if data.TokensPerSecond > 0 {
		tps = strconv.FormatFloat(data.TokensPerSecond, 'f', 2, 64)
	}
	if data.TimeToFirstToken > 0 {
		ttft = strconv.FormatFloat(data.TimeToFirstToken.Seconds(), 'f', 3, 64)
	}
	row := []string{
		now.Format("2006-01-02T15:04:05.000Z07:00"), info.method, info.path, strconv.Itoa(status), data.ModelName,
		optInt(data.PromptTokens), optInt(data.CompletionTokens), optInt(data.TotalTokens),
		tps, strconv.FormatBool(data.TPSEstimated), ttft,
		strconv.FormatFloat(data.Duration.Seconds(), 'f', 3, 64), strconv.FormatBool(complete),
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil // a request cut off by shutdown finished after Close
	}
	_ = r.w.Write(row) // write errors are sticky; returned by r.w.Error()
	r.w.Flush()
	return r.w.Error()
}

func (r *csvRecorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	r.w.Flush()
	return errors.Join(r.w.Error(), r.f.Close())
}

func formatMetricsLine(now time.Time, info *requestInfo, status int, data MetricsData, snap metricsSnapshot, complete bool) string {
	var b strings.Builder

	fmt.Fprintf(&b, "[%s] #%d %s %s %d",
		now.Format("15:04:05"), snap.QueryCount, info.method, escapeControls(info.path), status)

	if data.ModelName != "" {
		fmt.Fprintf(&b, " | Model: %s", escapeControls(truncateString(data.ModelName, 30)))
	}
	if data.PromptTokens > 0 || data.CompletionTokens > 0 {
		fmt.Fprintf(&b, " | Tokens: %d in / %d out", data.PromptTokens, data.CompletionTokens)
	} else if data.TotalTokens > 0 {
		fmt.Fprintf(&b, " | Tokens: %d", data.TotalTokens)
	}
	if data.TokensPerSecond > 0 {
		fmt.Fprintf(&b, " | TPS: %.2f", data.TokensPerSecond)
		if data.TPSEstimated {
			b.WriteString(" (est.)")
		}
	}
	if data.TimeToFirstToken > 0 {
		fmt.Fprintf(&b, " | TTFT: %.2fs", data.TimeToFirstToken.Seconds())
	}
	fmt.Fprintf(&b, " | Time: %.2fs", data.Duration.Seconds())

	fmt.Fprintf(&b, " | Avg: %.2fs", snap.AvgTimePerQuery.Seconds())
	if snap.AvgTokensPerSec > 0 {
		fmt.Fprintf(&b, " @ %.2f tok/s", snap.AvgTokensPerSec)
	}
	fmt.Fprintf(&b, " | Total tokens: %d", snap.TotalTokens)

	if !complete {
		b.WriteString(" | incomplete")
	}
	return b.String()
}

// truncateString truncates a string to max length in runes
func truncateString(s string, maxLen int) string {
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	return string(r[:maxLen-3]) + "..."
}
