package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
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
	ForwardManagement bool `yaml:"forward_management"`
	// AllowedHosts lists further names clients may give in the Host header,
	// besides those allowedHost always accepts. "*.example" matches the
	// subdomains of example, "*" any host.
	AllowedHosts []string      `yaml:"allowed_hosts"`
	Metrics      MetricsConfig `yaml:"metrics"`
	Logging      LoggingConfig `yaml:"logging"`
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
	TotalTokens     int64   // cumulative over all queries; int64 so it can't overflow on 32-bit builds
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
	TotalTokens     int64 // cumulative over all queries
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
	TPSEstimated     bool // TokensPerSecond derived by completeDerived, not reported
	// TimeToFirstToken is measured by the proxy for streamed responses
	// (TTFTMeasured; request start to the first generated output, so it
	// includes model loading) and otherwise as reported by the upstream;
	// 0 if unknown.
	TimeToFirstToken time.Duration
	TTFTMeasured     bool
	Duration         time.Duration
}

// maxCaptureBytes bounds how much of a response body (or of a single SSE
// line) is buffered for metrics parsing. Larger bodies are proxied as usual
// but not inspected.
const maxCaptureBytes = 16 << 20

// Wrapper inspection is separately bounded: each RawMessage decode scans and
// copies its subtree again. Share the work budget across both wrapper branches
// of a document, and reset it for the next JSON body, SSE event or NDJSON line.
const (
	maxWrapperDepth      = 8
	maxPayloadParseBytes = 2 * maxCaptureBytes
)

// A streamed body's first-to-last-byte time is used as generation time only
// if the body arrived in bursts: at least minStreamWaits pauses of streamGap
// or more between reads. A buffered body (short, or held back on the way) is
// read back to back from the transport's buffer, so the time between its
// reads measures only the proxy. Generation at over 1000 tokens/s has
// shorter pauses and falls back to the request duration too.
const (
	streamGap      = time.Millisecond
	minStreamWaits = 2
)

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
	debugf("config: proxy_port=%d upstream=%s:%d api_endpoints=%q forward_unmatched=%t forward_management=%t allowed_hosts=%q history_points=%d",
		cfg.ProxyPort, cfg.Server.Host, cfg.Server.Port, cfg.APIEndpoints, cfg.forwardsUnmatched(), cfg.ForwardManagement, cfg.AllowedHosts, cfg.Metrics.HistoryPoints)

	target := upstreamURL()
	proxy = newReverseProxy(target)

	// Setup signal handling for graceful shutdown: stop is done on the first
	// signal, force on the second. stop is derived from force, since forcing
	// implies stopping (not the other way round).
	force, forceNow := context.WithCancel(context.Background())
	defer forceNow()
	stop, stopNow := context.WithCancel(force)
	defer stopNow()
	setupSignalHandling(stopNow, forceNow)

	// Bind the port before announcing it or probing the upstream, so a port
	// in use fails at once. Connections arriving during the probe wait in the
	// listen backlog until serve() accepts them.
	ln, err := listen()
	if err != nil {
		fmt.Printf("Error starting proxy: %v\n", err)
		os.Exit(1)
	}
	// Open the CSV file only once the port is bound, so a port in use
	// doesn't leave a new file with only a header behind.
	if opts.csvPath != "" {
		closeCSV, err := setupCSVLog(opts.csvPath)
		if err != nil {
			fmt.Printf("Error opening CSV file: %v\n", err)
			os.Exit(1)
		}
		defer closeCSV()
	}
	fmt.Printf("Proxy listening on port %d\n", cfg.ProxyPort)
	if backend := detectBackend(target); backend != "" {
		fmt.Printf("Forwarding to %s at %s\n", backend, target.Host)
	} else {
		fmt.Printf("Forwarding to upstream at %s (server type not detected)\n", target.Host)
	}

	if err := runProxy(stop, force, ln); err != nil {
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
	return c, c.validate()
}

// validate rejects settings the defaults don't replace and that can't work.
func (c Config) validate() error {
	for _, p := range []struct {
		key  string
		port int
	}{{"proxy_port", c.ProxyPort}, {"server.port", c.Server.Port}} {
		if p.port < 1 || p.port > 65535 {
			return fmt.Errorf("%s: %d is not a port number (1-65535)", p.key, p.port)
		}
	}
	if err := checkServerHost(c.Server.Host); err != nil {
		return err
	}
	for _, e := range c.APIEndpoints {
		if !strings.HasPrefix(e, "/") {
			return fmt.Errorf(`api_endpoints: %q is not a path; patterns start with "/" ("/*" meters every path)`, e)
		}
	}
	return nil
}

// checkServerHost checks that h is a bare host name or IP address, as
// upstreamURL expects: net.JoinHostPort would turn "http://localhost" into
// "[http://localhost]:1234".
func checkServerHost(h string) error {
	if strings.Contains(h, "://") {
		return fmt.Errorf("server.host: %q: give the host name or IP address without a scheme (the upstream is always http)", h)
	}
	if _, _, err := net.SplitHostPort(h); err == nil {
		return fmt.Errorf("server.host: %q: give the port in server.port, not in the host", h)
	}
	if strings.ContainsAny(h, "[]") {
		return fmt.Errorf("server.host: %q: write IPv6 addresses without brackets", h)
	}
	notInHost := func(r rune) bool {
		return strings.ContainsRune("/?#@", r) || unicode.IsSpace(r) || unicode.IsControl(r)
	}
	if strings.ContainsFunc(h, notInHost) {
		return fmt.Errorf("server.host: %q is not a host name or IP address", h)
	}
	return nil
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
			return c, err // a syntax error in the second document
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
	client := &http.Client{
		Transport: upstreamTransport,
		Timeout:   2 * time.Second,
		// Probe the configured server only; a redirect shows as its 3xx status.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
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
			debugf("backend probe %s: body is not a JSON object: %s", path, escapeControls(err.Error()))
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
			debugf("backend probe %s: %s", p.path, escapeControls(err.Error()))
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

// listen opens the proxy's listening socket on cfg.ProxyPort, on all
// interfaces.
func listen() (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf(":%d", cfg.ProxyPort))
}

// runProxy serves the proxy on ln until stop is done (see serve).
func runProxy(stop, force context.Context, ln net.Listener) error {
	server := &http.Server{
		Handler: http.HandlerFunc(handleRequest),
		// No ReadTimeout/WriteTimeout: generations can take minutes, and an
		// expiring ReadTimeout cancels the request context mid-response.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
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
		done := sync.OnceFunc(func() { inFlight.Add(-1) })
		defer done()
		next.ServeHTTP(&hijackNotifier{ResponseWriter: w, onHijack: done}, r)
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

// hijackNotifier calls onHijack once the connection has been taken over for
// a protocol upgrade (forward_management: true). http.Server.Shutdown neither
// waits for nor closes hijacked connections, so serve stops counting such a
// request as in flight then. ReverseProxy hijacks and flushes through
// http.ResponseController, which finds the other methods via Unwrap.
type hijackNotifier struct {
	http.ResponseWriter
	onHijack func()
}

func (w *hijackNotifier) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *hijackNotifier) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, brw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.onHijack()
	}
	return conn, brw, err
}

// cleanPath reads backslashes as slashes (as WHATWG URL parsers do) and
// cleans the (decoded) request path p, so repeated slashes and "." segments,
// which upstream routers may accept, don't change how it is classified.
func cleanPath(p string) string {
	return path.Clean("/" + strings.ReplaceAll(p, `\`, "/"))
}

// isAPIEndpoint checks if the request path matches an API endpoint pattern.
// A trailing "*" in a pattern is ignored; patterns match as path prefixes of
// the cleaned path (cleanPath), keeping a trailing slash, so "//v1/chat" and
// "/./v1/chat" are metered like "/v1/chat".
func isAPIEndpoint(raw string) bool {
	p := cleanPath(raw)
	if p != "/" && (strings.HasSuffix(raw, "/") || strings.HasSuffix(raw, `\`)) {
		p += "/"
	}
	for _, pattern := range cfg.APIEndpoints {
		if strings.HasPrefix(p, strings.TrimSuffix(pattern, "*")) {
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
// managementEndpoints or below one. The path is cleaned (cleanPath, which
// also reads backslashes as slashes) and lowercased first, so repeated or
// trailing slashes and case variants that an upstream router may accept
// match too. Paths with ".." segments are refused before this check
// (hasDotDotSegment), since cleaning them can remove the endpoint's prefix.
func isManagementEndpoint(p string) bool {
	p = strings.ToLower(cleanPath(p))
	for _, e := range managementEndpoints {
		if p == e || strings.HasPrefix(p, e+"/") {
			return true
		}
	}
	return false
}

// hasDotDotSegment reports whether the (decoded) request path has a ".."
// segment, with backslashes also counted as separators. The proxy checks
// paths after path.Clean but forwards them as sent, so for such a path the
// two can differ: "/api/blobs/.." is checked as "/api", but a router that
// leaves dot segments alone can match it as "/api/blobs/{digest}", and one
// that resolves "\" as a separator can reach a path the check never saw.
// Clients resolve dot segments before sending, so these paths are refused
// whatever the configuration.
func hasDotDotSegment(p string) bool {
	isSep := func(r rune) bool { return r == '/' || r == '\\' }
	for seg := range strings.FieldsFuncSeq(p, isSep) {
		if seg == ".." {
			return true
		}
	}
	return false
}

// offersUpgrade reports whether h's Upgrade header offers a protocol other
// than h2c, i.e. asks to turn the connection into a tunnel (WebSocket).
func offersUpgrade(h http.Header) bool {
	for _, v := range h.Values("Upgrade") {
		for p := range strings.SplitSeq(v, ",") {
			if p = strings.TrimSpace(p); p != "" && !strings.EqualFold(p, "h2c") {
				return true
			}
		}
	}
	return false
}

// localSuffixes are domains reserved for local use (RFC 6761, RFC 6762,
// ICANN's .internal, RFC 8375), which public DNS can't answer for. ".lan" is
// left out: it is not reserved and could still be delegated.
var localSuffixes = []string{".localhost", ".local", ".internal", ".home.arpa"}

// hostName returns the host in a Host header value (host or host:port)
// without port, IPv6 brackets and trailing dot, in lowercase.
func hostName(hostport string) string {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = strings.TrimSuffix(strings.TrimPrefix(hostport, "["), "]") // no port
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

// allowedHost reports whether host (from hostName) names the proxy in a way
// a DNS rebinding attack can't. The proxy replaces the Host header when
// forwarding, so the upstream's own check never sees it. Like Ollama's own
// check, it accepts an empty host, IP addresses, localhost, names in
// localSuffixes and this machine's hostname, plus cfg.AllowedHosts.
func allowedHost(host string) bool {
	if _, err := netip.ParseAddr(host); err == nil || host == "" {
		return true // an IP address involves no DNS lookup to rebind
	}
	if host == "localhost" || hasSuffix(host, localSuffixes) {
		return true
	}
	if name, err := os.Hostname(); err == nil && host == strings.ToLower(name) {
		return true
	}
	return cfg.allowsHost(host)
}

// hasSuffix reports whether s ends with one of suffixes.
func hasSuffix(s string, suffixes []string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(s, suffix) {
			return true
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

// hasPathTarget reports whether a request's target is a path: the usual
// origin form ("/v1/chat"), or an absolute URL with a path. Other targets
// leave URL.Path empty or not starting with "/", so the path checks would
// pass while the upstream sees something else: an opaque target such as
// "a:x:/api/delete" is forwarded as "x:/api/delete", which a Go upstream
// (Ollama) routes to /api/delete. Clients send only paths to a reverse proxy.
func hasPathTarget(u *url.URL) bool {
	return u.Opaque == "" && strings.HasPrefix(u.Path, "/")
}

// redactedTarget returns a request target for logging without user info,
// query string or fragment, which may carry passwords or keys.
func redactedTarget(u *url.URL) string {
	c := *u
	c.User, c.RawQuery, c.ForceQuery, c.Fragment, c.RawFragment = nil, "", false, "", ""
	return c.String()
}

// allowsHost reports whether host (lowercase, without port) matches an
// allowed_hosts entry.
func (c Config) allowsHost(host string) bool {
	for _, entry := range c.AllowedHosts {
		entry = strings.ToLower(entry)
		if entry == "*" || entry == host || strings.HasPrefix(entry, "*.") && strings.HasSuffix(host, entry[1:]) {
			return true
		}
	}
	return false
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
	if !hasPathTarget(r.URL) {
		fmt.Fprintf(out, "[Proxy] Rejected %s %s: request target is not a path\n", r.Method, escapeControls(redactedTarget(r.URL)))
		http.Error(w, "Bad Request: request target is not a path", http.StatusBadRequest)
		return
	}
	if host := hostName(r.Host); !allowedHost(host) {
		fmt.Fprintf(out, "[Proxy] Rejected %s %s: Host %q not allowed (add %q to allowed_hosts if clients use this name)\n",
			r.Method, escapeControls(r.URL.Path), r.Host, host)
		http.Error(w, "Forbidden: Host not allowed", http.StatusForbidden)
		return
	}
	if hasDotDotSegment(r.URL.Path) {
		fmt.Fprintf(out, "[Proxy] Rejected %s %s: \"..\" segment in path\n", r.Method, escapeControls(r.URL.Path))
		http.Error(w, `Bad Request: ".." segment in path`, http.StatusBadRequest)
		return
	}
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
	// escape a client's (possibly long) path unless it is printed. The query
	// string is left out: it may carry keys or other private values.
	if cfg.Logging.Debug {
		debugf("req %d: %s %s from %s (metered: %t)",
			info.id, r.Method, escapeControls(r.URL.Path), r.RemoteAddr, metered)
	}
	ctx := context.WithValue(r.Context(), requestInfoKey{}, info)
	proxy.ServeHTTP(w, r.WithContext(ctx))
}

// Limits on upstream response headers. Real LLM servers send a few hundred
// bytes; Go's default allows 10 MiB of headers, and parsing a Content-Type
// with thousands of MIME parameters allocates a map for each, before the
// body capture limits apply.
const (
	maxResponseHeaderBytes = 64 << 10
	maxContentTypeBytes    = 1 << 10
)

// upstreamTransport carries all requests to the upstream: proxied requests
// and the startup probes. It is http.DefaultTransport except that it
// ignores HTTP_PROXY and friends (the upstream is a local or LAN server,
// which an ambient proxy setting would otherwise route elsewhere; Go only
// exempts localhost), keeps more idle connections to the single upstream
// (the default keeps 2), and limits response headers.
var upstreamTransport = newUpstreamTransport()

func newUpstreamTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.MaxIdleConnsPerHost = 16
	t.MaxResponseHeaderBytes = maxResponseHeaderBytes
	return t
}

// newReverseProxy returns a reverse proxy to target that records metrics
// from response bodies while streaming them to the client.
func newReverseProxy(target *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Transport: upstreamTransport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// SetURL keeps user info from an absolute-form request target
			// ("http://user:pass@host/…"). It isn't sent, but drop it so it
			// can't reach logs.
			pr.Out.URL.User = nil
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
	// Checked first, for unmetered responses too: ReverseProxy parses the
	// Content-Type with mime.ParseMediaType after this hook returns.
	contentType := resp.Header.Get("Content-Type")
	if len(contentType) > maxContentTypeBytes {
		return fmt.Errorf("upstream Content-Type header too long (%d bytes, limit %d)", len(contentType), maxContentTypeBytes)
	}
	info, ok := resp.Request.Context().Value(requestInfoKey{}).(*requestInfo)
	if !ok {
		return nil
	}
	if cfg.Logging.Debug { // only escape the path when it is printed (see handleRequest)
		u := resp.Request.URL // without the query string, as in handleRequest
		debugf("req %d: upstream %s://%s%s -> %d after %.2fs, Content-Type %q",
			info.id, u.Scheme, u.Host, escapeControls(u.Path), resp.StatusCode, time.Since(info.start).Seconds(), contentType)
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
	capture   capture   // nil when the body is not inspected
	firstByte time.Time // when the first body bytes were read
	lastByte  time.Time // when the last body bytes were read
	waits     int       // pauses of streamGap or more between reads with data
	bytes     int64
	eof       bool
	once      sync.Once
}

func (b *meteredBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.bytes += int64(n)
		now := time.Now()
		if b.firstByte.IsZero() {
			b.firstByte = now
		} else if now.Sub(b.lastByte) >= streamGap {
			b.waits++
		}
		b.lastByte = now
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
			// Left 0 for a body that arrived at once (see streamGap), so
			// completeDerived falls back to the request duration.
			if b.waits >= minStreamWaits {
				streamTime = b.lastByte.Sub(b.firstByte)
			}
		}
		data.Duration = end.Sub(b.info.start)
		// TPS is estimated with the upstream's TTFT, before it is replaced:
		// the measured one includes model loading and, for a body held back
		// on the way, the whole generation.
		basis := data.completeDerived(streamTime)
		if b.capture != nil {
			if at := b.capture.firstOutput(); !at.IsZero() {
				data.TimeToFirstToken, data.TTFTMeasured = at.Sub(b.info.start), true
			}
		}

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
	if data.TTFTMeasured {
		debugf("req %d: first output in document %d, %.2fs after the request (measured TTFT, includes model loading)",
			id, d.outputDoc, data.TimeToFirstToken.Seconds())
	}
	if basis != "" {
		debugf("req %d: no tokens/s reported; estimated as %s", id, basis)
	}
	if !data.hasInference() {
		debugf("req %d: no model, token or rate fields found; not counted as a query", id)
	} else if d.name == "SSE" && data.TotalTokens == 0 && b.eof {
		// Not before EOF: usage comes in the last events, which may not have arrived.
		debugf("req %d: stream has no token usage%s", id, usageHint(b.info.path))
	}
}

// usageHint explains a missing token usage in a complete SSE stream from
// path. OpenAI-style completions endpoints only send usage on request; other
// APIs (LM Studio native, Responses) always do, so for them nothing is added.
func usageHint(p string) string {
	if strings.HasSuffix(cleanPath(p), "/completions") {
		return `; OpenAI-compatible clients must send "stream_options": {"include_usage": true}`
	}
	return ""
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
	// firstOutput returns when the first document carrying generated output
	// was written, or the zero time if none was (or for unstreamed bodies).
	firstOutput() time.Time
}

// captureDiag counts the JSON documents (whole body, SSE events or NDJSON
// lines) a capture has seen.
type captureDiag struct {
	name      string // "JSON", "SSE" or "NDJSON"
	documents int    // parsed as JSON objects
	invalid   int    // not valid JSON objects
	oversized int    // bodies, lines or events skipped for exceeding maxCaptureBytes
	outputDoc int    // number (from 1) of the first document with generated output; 0 if none
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
	// Only the media type matters; parameters such as charset are ignored,
	// so they aren't parsed.
	mediaType, _, _ := strings.Cut(contentType, ";")
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
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

// firstOutput is zero: a JSON body arrives whole, after generation ends.
func (c *jsonCapture) firstOutput() time.Time { return time.Time{} }

// outputTimer notes when a streamed body's first document carrying generated
// output (text, reasoning or tool calls) was written to its capture. Captures
// are written as the proxy reads the body from the upstream, just before it
// forwards each chunk, so this times the upstream (model loading included),
// not a slow client's receipt of the token.
type outputTimer struct {
	at  time.Time
	doc int // number of that document among those parsed, from 1
}

// note records the parse result of one document; parsed counts the
// documents parsed so far, this one included.
func (o *outputTimer) note(output bool, parsed int) {
	if output && o.at.IsZero() {
		o.at, o.doc = time.Now(), parsed
	}
}

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
// The line is held with one byte to spare for a "\r" before the "\n", so a
// CRLF line of maxCaptureBytes is not dropped; without the "\r" it must fit.
func (s *lineSplitter) endLine(emit func(line []byte)) {
	if line := bytes.TrimSuffix(s.line, []byte("\r")); !s.skipLine && len(line) > maxCaptureBytes {
		s.oversized++
	} else if !s.skipLine {
		emit(line)
	}
	s.line, s.skipLine = reuse(s.line), false
}

func (s *lineSplitter) appendLine(p []byte) {
	if s.skipLine {
		return
	}
	if len(s.line)+len(p) > maxCaptureBytes+1 { // +1: room for a "\r"
		s.line, s.skipLine = nil, true
		s.oversized++
		return
	}
	s.line = append(s.line, p...)
}

// reuseLimit is the largest buffer a capture keeps for the next line or
// event. Larger ones are dropped, so one large event doesn't keep up to
// maxCaptureBytes allocated for the rest of a long stream.
const reuseLimit = 64 << 10

// reuse returns b emptied for reuse, or nil if its backing array is larger
// than reuseLimit.
func reuse(b []byte) []byte {
	if cap(b) > reuseLimit {
		return nil
	}
	return b[:0]
}

// sseCapture parses a text/event-stream body event by event, so memory use
// is bounded by the largest event rather than the whole stream. Metrics from
// later events override earlier ones.
type sseCapture struct {
	lines   lineSplitter
	data    []byte // data field of the event being assembled
	skip    bool   // part of the event exceeded maxCaptureBytes; drop the rest of it
	dropped int    // lines.oversized at the last noteDropped
	metrics MetricsData
	d       captureDiag // oversized counts skipped events, once per event
	output  outputTimer
}

func (c *sseCapture) Write(p []byte) (int, error) {
	c.lines.write(p, c.processLine)
	return len(p), nil
}

func (c *sseCapture) processLine(line []byte) {
	c.noteDropped()
	switch {
	case len(line) == 0:
		c.dispatch()
	case c.skip:
		// Data lines after the oversized part would be parsed as a fragment.
	case bytes.HasPrefix(line, []byte("data:")):
		value := bytes.TrimPrefix(line[len("data:"):], []byte(" "))
		sep := 0 // the "\n" joining this data line to earlier ones
		if len(c.data) > 0 {
			sep = 1
		}
		if len(c.data)+sep+len(value) > maxCaptureBytes {
			c.skipEvent()
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

// noteDropped skips the event being assembled if the splitter has dropped an
// oversized line since the last call. The splitter doesn't emit dropped
// lines, so this runs before each emitted line and before the final dispatch
// in finish, where the dropped line may have been the last one.
func (c *sseCapture) noteDropped() {
	if c.lines.oversized != c.dropped {
		c.dropped = c.lines.oversized
		c.skipEvent()
	}
}

// skipEvent drops the event being assembled up to its closing blank line, so
// its remaining data lines aren't parsed as a fragment. An event counts as
// oversized once, however many of its lines exceeded the limit.
func (c *sseCapture) skipEvent() {
	if !c.skip {
		c.d.oversized++
	}
	c.data, c.skip = nil, true
}

func (c *sseCapture) dispatch() {
	if !c.skip && len(c.data) > 0 && !bytes.Equal(c.data, []byte("[DONE]")) {
		ok, output := c.metrics.mergeDocument(c.data)
		c.d.count(ok)
		c.output.note(output, c.d.documents)
	}
	c.data, c.skip = reuse(c.data), false
}

func (c *sseCapture) finish() MetricsData {
	c.lines.flush(c.processLine)
	c.noteDropped()
	c.dispatch()
	return c.metrics
}

func (c *sseCapture) diag() captureDiag {
	d := c.d
	d.name = "SSE"
	d.outputDoc = c.output.doc
	return d
}

func (c *sseCapture) firstOutput() time.Time { return c.output.at }

// ndjsonCapture parses a newline-delimited JSON body (Ollama's native
// streaming format) line by line. Metrics from later lines override earlier
// ones; Ollama reports its stats in the final "done" line.
type ndjsonCapture struct {
	lines   lineSplitter
	metrics MetricsData
	d       captureDiag
	output  outputTimer
}

func (c *ndjsonCapture) Write(p []byte) (int, error) {
	c.lines.write(p, c.processLine)
	return len(p), nil
}

func (c *ndjsonCapture) processLine(line []byte) {
	if len(bytes.TrimSpace(line)) > 0 {
		ok, output := c.metrics.mergeDocument(line)
		c.d.count(ok)
		c.output.note(output, c.d.documents)
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
	d.outputDoc = c.output.doc
	return d
}

func (c *ndjsonCapture) firstOutput() time.Time { return c.output.at }

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
	// Generated output, used only to time the first token of a stream.
	Choices  outputChoices `json:"choices"`  // OpenAI chat and legacy completions chunks
	Message  *outputFields `json:"message"`  // Ollama native /api/chat
	Content  string        `json:"content"`  // LM Studio API v1 message.delta, reasoning.delta
	Delta    string        `json:"delta"`    // OpenAI Responses API *.delta events
	Thinking string        `json:"thinking"` // Ollama native /api/generate
	// Ollama native /api/generate puts its text in "response" (a string).
	// Wrapped payloads: the API v1 "chat.end" SSE event carries the response
	// in "result", OpenAI Responses API events in "response". Kept raw so an
	// unrelated field of the same name doesn't fail the whole parse.
	Result   json.RawMessage `json:"result"`
	Response json.RawMessage `json:"response"`
}

// outputFields are the fields of an OpenAI chunk's delta or an Ollama
// message that carry generated output.
type outputFields struct {
	Content          string        `json:"content"`
	ReasoningContent string        `json:"reasoning_content"` // LM Studio, DeepSeek, llama.cpp
	Reasoning        string        `json:"reasoning"`         // Ollama OpenAI-compatible
	Thinking         string        `json:"thinking"`          // Ollama native
	ToolCalls        nonEmptyArray `json:"tool_calls"`
}

func (f *outputFields) hasOutput() bool {
	return f != nil && (f.Content != "" || f.ReasoningContent != "" || f.Reasoning != "" ||
		f.Thinking != "" || bool(f.ToolCalls))
}

// nonEmptyArray records whether a JSON value is an array with at least one
// element, without decoding the elements: a document within maxCaptureBytes
// can hold millions of them.
type nonEmptyArray bool

func (a *nonEmptyArray) UnmarshalJSON(b []byte) error {
	// json.Unmarshal has checked the syntax, so "[" is followed by "]" or a value.
	b = bytes.TrimSpace(b)
	*a = len(b) > 1 && b[0] == '[' && bytes.TrimSpace(b[1:])[0] != ']'
	return nil
}

// outputChoices records whether one of the first maxOutputChoices elements
// of an OpenAI "choices" array carries generated output. Elements are decoded
// one at a time, so neither memory nor work grows with the number of elements
// a document within maxCaptureBytes can hold (millions). Responses have one
// choice per requested completion ("n", usually 1). Anything but an array is
// ignored.
//
// Only the first "choices" member of a document is inspected. json.Unmarshal
// calls UnmarshalJSON for every repeat of a key, and each call started a new
// decoder with its own allowance, so a document of a million tiny repeated
// "choices" members within maxCaptureBytes allocated hundreds of MiB. Plain
// JSON fields keep the last repeat instead; servers don't repeat keys.
type outputChoices struct {
	seen, output bool
}

const maxOutputChoices = 16

func (o *outputChoices) UnmarshalJSON(b []byte) error {
	if o.seen {
		return nil
	}
	o.seen = true
	dec := json.NewDecoder(bytes.NewReader(b))
	if t, err := dec.Token(); err != nil || t != json.Delim('[') {
		return nil
	}
	for i := 0; i < maxOutputChoices && dec.More(); i++ {
		var c struct {
			Delta *outputFields `json:"delta"` // chat completions chunk
			Text  string        `json:"text"`  // legacy completions chunk
		}
		// A type error skips only the mismatched field, as in json.Unmarshal.
		// json.Unmarshal has ruled out syntax errors; one would end More().
		_ = dec.Decode(&c)
		if c.Text != "" || c.Delta.hasOutput() {
			o.output = true
			return nil
		}
	}
	return nil
}

// hasOutput reports whether p, as a streamed document, carries generated
// output. Events sent before generation, such as LM Studio's chat.start and
// prompt_processing.* or an OpenAI chunk with only the role, don't. Wrapped
// payloads are not inspected: they summarize the response at its end.
func (p *llmPayload) hasOutput() bool {
	// "response" is a string in Ollama's /api/generate, an object (a
	// wrapper) in Responses API events; `""` is the empty string.
	return p.Choices.output || p.Message.hasOutput() || p.Content != "" || p.Delta != "" || p.Thinking != "" ||
		(len(p.Response) > 2 && p.Response[0] == '"')
}

// mergeJSON applies the metrics found in a JSON document to d and reports
// whether it was a JSON object. Other documents are ignored; a field of
// unexpected type is skipped without discarding the others.
func (d *MetricsData) mergeJSON(raw []byte) bool {
	ok, _ := d.mergeDocument(raw)
	return ok
}

// mergeDocument is mergeJSON that also reports whether the document carries
// generated output (see llmPayload.hasOutput), for streamed bodies.
func (d *MetricsData) mergeDocument(raw []byte) (ok, output bool) {
	remaining := maxPayloadParseBytes
	p, ok := d.mergeJSONPayload(raw, 0, &remaining)
	return ok, ok && p.hasOutput()
}

// mergeJSONBounded keeps already merged fields when deeper inspection reaches
// a limit. This only limits metrics inspection; response bytes are forwarded.
func (d *MetricsData) mergeJSONBounded(raw []byte, depth int, remaining *int) bool {
	_, ok := d.mergeJSONPayload(raw, depth, remaining)
	return ok
}

// mergeJSONPayload is mergeJSONBounded that also returns the decoded
// payload, without its wrappers' contents.
func (d *MetricsData) mergeJSONPayload(raw []byte, depth int, remaining *int) (*llmPayload, bool) {
	if depth > maxWrapperDepth || len(raw) > *remaining {
		return nil, false
	}
	*remaining -= len(raw)
	var p llmPayload
	var typeErr *json.UnmarshalTypeError
	err := json.Unmarshal(raw, &p)
	// A type error without a field is the document itself (array, string, ...).
	if err != nil && (!errors.As(err, &typeErr) || typeErr.Field == "") {
		return nil, false
	}
	d.merge(&p)
	for _, wrapped := range []json.RawMessage{p.Result, p.Response} {
		if len(wrapped) > 0 {
			d.mergeJSONBounded(wrapped, depth+1, remaining)
		}
	}
	return &p, true
}

// merge applies the metrics in p to d. Fields p reports override those already
// in d; fields it lacks are left as is. mergeJSONBounded handles wrappers.
// Limits on numbers from the upstream. Values beyond them are ignored as if
// they weren't reported: JSON allows integers up to math.MaxInt and floats up
// to 1.8e308, which overflowed token sums and the TTFT's time.Duration, made
// the average rate +Inf and printed 300-digit numbers. Real values are far
// below: context windows hold about a million tokens, and rates are at most
// thousands of tokens per second. maxTokenCount is 2^29 so that prompt plus
// completion tokens fit in a 32-bit int.
const (
	maxTokenCount      = 1 << 29
	maxTokensPerSecond = 1e6
	maxTTFTSeconds     = 24 * 60 * 60
)

// plausibleTPS reports whether v is a usable tokens-per-second value.
func plausibleTPS(v float64) bool { return v > 0 && v <= maxTokensPerSecond }

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
		if plausibleTPS(s.TokensPerSecond) {
			d.TokensPerSecond = s.TokensPerSecond
		}
		if ttft := max(s.TimeToFirstToken, s.TimeToFirstTokenSeconds); ttft > 0 && ttft <= maxTTFTSeconds {
			d.TimeToFirstToken = time.Duration(ttft * float64(time.Second))
		}
		setFirstPositive(&d.PromptTokens, s.InputTokens)
		setFirstPositive(&d.CompletionTokens, s.TotalOutputTokens)
	}
	if t := p.Timings; t != nil && plausibleTPS(t.PredictedPerSecond) {
		d.TokensPerSecond = t.PredictedPerSecond
	}
	setFirstPositive(&d.PromptTokens, p.PromptEvalCount)
	setFirstPositive(&d.CompletionTokens, p.EvalCount)
	if p.EvalCount > 0 && p.EvalCount <= maxTokenCount && p.EvalDuration > 0 {
		if tps := float64(p.EvalCount) / time.Duration(p.EvalDuration).Seconds(); plausibleTPS(tps) {
			d.TokensPerSecond = tps
		}
	}
}

// setFirstPositive sets *dst to the first value in vals between 1 and
// maxTokenCount, leaving it unchanged if there is none.
func setFirstPositive(dst *int, vals ...int) {
	for _, v := range vals {
		if v > 0 && v <= maxTokenCount {
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
	tps := float64(d.CompletionTokens) / genTime.Seconds()
	if !plausibleTPS(tps) {
		return ""
	}
	d.TokensPerSecond = tps
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
	metrics.TotalTokens += int64(data.TotalTokens)
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
// New columns go at the end: existing files are appended to as they are.
var csvHeader = []string{
	"timestamp", "method", "path", "status", "model",
	"prompt_tokens", "completion_tokens", "total_tokens",
	"tokens_per_second", "tps_estimated", "ttft_seconds", "duration_seconds", "complete",
	"ttft_measured",
}

// csvRecorder appends one row per recorded query to a CSV file. Each row is
// written with a single Write, so the file is usable while the proxy runs.
type csvRecorder struct {
	mu      sync.Mutex
	f       csvFile
	closed  bool
	partial int64 // bytes of a partly written row still at the end of f
}

// csvFile is the part of *os.File that csvRecorder uses (replaced in tests).
type csvFile interface {
	io.WriteCloser
	Stat() (os.FileInfo, error)
	Truncate(size int64) error
}

// setupCSVLog opens path as csvLog, as given by --csv, and announces it.
// The returned function closes it.
func setupCSVLog(path string) (func(), error) {
	rec, err := openCSVLog(path)
	if err != nil {
		return nil, err
	}
	csvLog = rec
	fmt.Fprintf(out, "Writing statistics to %s\n", path)
	return func() { _ = rec.Close() }, nil // close errors at exit have no one to report to
}

// openCSVLog opens path for appending, creating it if needed. The header is
// written only when the file is empty, so restarts keep adding to one file.
//
// A new file is readable by its owner only (0600 before the umask): rows show
// what was queried and when. The mode of an existing file is left alone.
func openCSVLog(path string) (*csvRecorder, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	r := &csvRecorder{f: f}
	if st, err := f.Stat(); err != nil || st.Size() == 0 {
		if err := r.writeRow(csvHeader); err != nil {
			f.Close()
			return nil, err
		}
	}
	return r, nil
}

// writeRow encodes row as CSV and writes it with a single Write. A shared
// csv.Writer would keep the first write error forever (its bufio.Writer does),
// so one failed write, e.g. while Excel locks the file on Windows or the disk
// is briefly full, would stop CSV recording until a restart.
//
// A write that fails after writing part of the row (a full disk) leaves an
// unfinished record, possibly inside a quoted field, that would swallow the
// next rows. Those bytes are cut off the file before anything else is
// written; until that succeeds, rows are refused rather than merged.
func (r *csvRecorder) writeRow(row []string) error {
	if err := r.dropPartial(); err != nil {
		return err
	}
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write(row) // writes to a bytes.Buffer can't fail
	w.Flush()
	n, err := r.f.Write(buf.Bytes())
	if err != nil && n > 0 {
		r.partial = int64(n)
		err = errors.Join(err, r.dropPartial())
	}
	return err
}

// dropPartial truncates the bytes of a partly written row off the end of the
// file. The file is opened for appending, so they are its last bytes.
func (r *csvRecorder) dropPartial() error {
	if r.partial == 0 {
		return nil
	}
	st, err := r.f.Stat()
	if err == nil {
		err = r.f.Truncate(st.Size() - r.partial)
	}
	if err != nil {
		return fmt.Errorf("removing a partly written row: %w", err)
	}
	r.partial = 0
	return nil
}

// csvText keeps upstream/client text from becoming a spreadsheet formula.
// Inspect past leading whitespace, controls and format characters, but retain
// the original value after the protective apostrophe. CSV quoting alone only
// protects field boundaries, not spreadsheet interpretation.
//
// The same check runs after every ";" in s: Excel in locales whose list
// separator is ";" (Finnish, German, …) splits the comma-separated file at
// semicolons and ignores the quoting, so the text after one starts a new
// cell. Such a part gets an apostrophe after the ";" when it would start a
// formula. Text without ";" is returned without copying when it needs none.
func csvText(s string) string {
	if !utf8.ValidString(s) || startsFormula(s) {
		s = "'" + s
	}
	if !strings.Contains(s, ";") {
		return s
	}
	var b strings.Builder
	for i, part := range strings.Split(s, ";") {
		if i > 0 {
			b.WriteByte(';')
			if startsFormula(part) {
				b.WriteByte('\'')
			}
		}
		b.WriteString(part)
	}
	return b.String()
}

// startsFormula reports whether a spreadsheet could read s as a formula: s
// starts with a control character, or with a formula prefix after leading
// whitespace, controls, format characters and double quotes (Excel reads a
// quoted cell "=…" as a formula too).
func startsFormula(s string) bool {
	if first, _ := utf8.DecodeRuneInString(s); unicode.IsControl(first) {
		return true
	}
	trimmed := strings.TrimLeftFunc(s, func(r rune) bool {
		return r == '"' || unicode.IsSpace(r) || unicode.IsControl(r) || unicode.In(r, unicode.Cf)
	})
	first, _ := utf8.DecodeRuneInString(trimmed)
	switch first {
	case '=', '+', '-', '@', '＝', '＋', '－', '＠':
		return true
	}
	return false
}

func (r *csvRecorder) record(now time.Time, info *requestInfo, status int, data MetricsData, complete bool) error {
	optInt := func(n int) string {
		if n <= 0 {
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
		now.Format("2006-01-02T15:04:05.000Z07:00"), csvText(info.method), csvText(info.path), strconv.Itoa(status), csvText(data.ModelName),
		optInt(data.PromptTokens), optInt(data.CompletionTokens), optInt(data.TotalTokens),
		tps, strconv.FormatBool(data.TPSEstimated), ttft,
		strconv.FormatFloat(data.Duration.Seconds(), 'f', 3, 64), strconv.FormatBool(complete),
		strconv.FormatBool(data.TTFTMeasured),
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil // a request cut off by shutdown finished after Close
	}
	return r.writeRow(row)
}

func (r *csvRecorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	return r.f.Close()
}

func formatMetricsLine(now time.Time, info *requestInfo, status int, data MetricsData, snap metricsSnapshot, complete bool) string {
	var b strings.Builder

	fmt.Fprintf(&b, "[%s] #%d %s %s %d",
		now.Format("15:04:05"), snap.QueryCount, info.method, escapeControls(info.path), status)

	if data.ModelName != "" {
		fmt.Fprintf(&b, " | Model: %s", escapeTruncated(data.ModelName, 30))
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

// escapeTruncated escapes s as escapeControls does and shortens the result
// to at most maxLen runes, ending in "..." if it was cut. It cuts between the
// runes (or invalid bytes) of s, so escapes are kept whole and count with
// their full length, and invalid bytes are escaped rather than replaced by
// U+FFFD. maxLen must be at least 3.
//
// It reads s only until it knows the result needs cutting, so the work is
// bounded by maxLen, not by len(s): every status line shows the model name,
// which comes from the upstream and may be up to maxCaptureBytes long.
func escapeTruncated(s string, maxLen int) string {
	width := 0 // runes of the escaped s[:i]
	cut := -1  // bytes of s whose escape fits in maxLen-3 runes, once known
	var esc [16]byte
	for i := 0; i < len(s); {
		n, escape := escapeAt(s, i)
		w := 1
		if escape {
			w = len(appendGoEscape(esc[:0], s[i:i+n])) // escapes are ASCII
		}
		if cut < 0 && width+w > maxLen-3 {
			cut = i
		}
		if width += w; width > maxLen {
			return escapeControls(s[:cut]) + "..."
		}
		i += n
	}
	return escapeControls(s)
}
