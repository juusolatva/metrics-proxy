package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.yaml.in/yaml/v4"
)

// Config holds the proxy configuration
type Config struct {
	ProxyPort    int            `yaml:"proxy_port"`
	LmStudio     LmStudioConfig `yaml:"lm_studio"`
	APIEndpoints []string       `yaml:"api_endpoints"`
	Metrics      MetricsConfig  `yaml:"metrics"`
	Logging      LoggingConfig  `yaml:"logging"`
}

type LmStudioConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

type MetricsConfig struct {
	Mode              string `yaml:"mode"`
	RefreshIntervalMs int    `yaml:"refresh_interval_ms"`
	HistoryPoints     int    `yaml:"history_points"`
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
	TimeToFirstToken time.Duration // as reported by the upstream; 0 if unknown
	Duration         time.Duration
}

// maxCaptureBytes bounds how much of a response body (or of a single SSE
// line) is buffered for metrics parsing. Larger bodies are proxied as usual
// but not inspected.
const maxCaptureBytes = 16 << 20

// Global config and metrics
var (
	cfg     Config
	proxy   *httputil.ReverseProxy
	metrics Metrics
	out     io.Writer = os.Stdout // destination for per-request status lines
)

func main() {
	fmt.Println("=== LM Studio Metrics Proxy ===")

	// Load configuration
	if err := loadConfig(); err != nil {
		fmt.Printf("Error loading config: %v\n", err)
		os.Exit(1)
	}

	target := upstreamURL()
	proxy = newReverseProxy(target)

	// Setup signal handling for graceful shutdown
	go setupSignalHandling()

	// Start proxy server
	fmt.Printf("Proxy listening on port %d\n", cfg.ProxyPort)
	fmt.Printf("Forwarding to LM Studio at %s\n", target.Host)

	if err := runProxy(); err != nil {
		fmt.Printf("Error running proxy: %v\n", err)
		os.Exit(1)
	}
}

func loadConfig() error {
	data, err := os.ReadFile("config.yaml")
	if err != nil {
		return fmt.Errorf("failed to read config file: %w", err)
	}

	c, err := parseConfig(data)
	if err != nil {
		return err
	}
	cfg = c
	return nil
}

// parseConfig unmarshals YAML configuration and fills in defaults for unset fields.
func parseConfig(data []byte) (Config, error) {
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("failed to parse config file: %w", err)
	}

	if c.ProxyPort == 0 {
		c.ProxyPort = 8080
	}
	if c.LmStudio.Host == "" {
		c.LmStudio.Host = "localhost"
	}
	if c.LmStudio.Port == 0 {
		c.LmStudio.Port = 1234
	}
	if c.Metrics.HistoryPoints <= 0 {
		c.Metrics.HistoryPoints = 10
	}
	return c, nil
}

// upstreamURL returns the base URL of the configured LM Studio instance.
func upstreamURL() *url.URL {
	return &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(cfg.LmStudio.Host, strconv.Itoa(cfg.LmStudio.Port)),
	}
}

func setupSignalHandling() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		fmt.Println("\nShutting down...")
		os.Exit(0)
	}()
}

func runProxy() error {
	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.ProxyPort),
		Handler: http.HandlerFunc(handleRequest),
		// No ReadTimeout/WriteTimeout: generations can take minutes, and an
		// expiring ReadTimeout cancels the request context mid-response.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	fmt.Println("Press Ctrl+C to stop")
	return server.ListenAndServe()
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

// requestInfo describes an incoming request; it travels to the response
// side of the proxy via the request context.
type requestInfo struct {
	start  time.Time
	method string
	path   string
}

type requestInfoKey struct{}

// handleRequest processes incoming requests
func handleRequest(w http.ResponseWriter, r *http.Request) {
	if !isAPIEndpoint(r.URL.Path) {
		fmt.Fprintf(out, "[Proxy] Non-API request: %s %s\n", r.Method, r.URL.Path)
		http.NotFound(w, r)
		return
	}

	info := &requestInfo{start: time.Now(), method: r.Method, path: r.URL.Path}
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
	// A 101 response body is used for the upgraded connection and must not be wrapped.
	if !ok || resp.StatusCode == http.StatusSwitchingProtocols {
		return nil
	}

	resp.Body = &meteredBody{
		ReadCloser: resp.Body,
		info:       info,
		status:     resp.StatusCode,
		capture:    newCapture(resp.Header.Get("Content-Type")),
	}
	return nil
}

// proxyError reports upstream failures to the client as 502 Bad Gateway.
func proxyError(w http.ResponseWriter, r *http.Request, err error) {
	fmt.Fprintf(out, "[ERROR] %s %s: %v\n", r.Method, r.URL.Path, err)
	http.Error(w, "Failed to connect to LM Studio", http.StatusBadGateway)
}

// meteredBody passes an upstream response body through to the reverse proxy,
// feeding a copy to a capture, and records metrics when the body ends.
type meteredBody struct {
	io.ReadCloser
	info      *requestInfo
	status    int
	capture   capture // nil when the body is not inspected
	firstByte time.Time
	eof       bool
	once      sync.Once
}

func (b *meteredBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		if b.firstByte.IsZero() {
			b.firstByte = time.Now()
		}
		if b.capture != nil {
			b.capture.Write(p[:n])
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
		if _, ok := b.capture.(*sseCapture); ok && !b.firstByte.IsZero() {
			streamTime = end.Sub(b.firstByte)
		}
		data.Duration = end.Sub(b.info.start)
		data.completeDerived(streamTime)

		recordQuery(b.info, b.status, data, b.eof)
	})
}

// recordQuery updates the global metrics and prints a status line. Responses
// without inference data (model listings, errors, ...) are only logged.
func recordQuery(info *requestInfo, status int, data MetricsData, complete bool) {
	if !data.hasInference() {
		fmt.Fprintf(out, "[Proxy] %s %s %d (%.2fs)\n",
			info.method, info.path, status, data.Duration.Seconds())
		return
	}

	snap := updateGlobalMetrics(data)
	displayMetrics(info, status, data, snap, complete)
}

// capture inspects a response body as it is streamed to the client.
type capture interface {
	io.Writer
	// finish returns the metrics found in everything written so far.
	finish() MetricsData
}

// newCapture returns a capture for bodies of the given Content-Type, or nil
// if bodies of that type carry no metrics.
func newCapture(contentType string) capture {
	mediaType, _, _ := mime.ParseMediaType(contentType)
	switch {
	case mediaType == "text/event-stream":
		return &sseCapture{}
	case mediaType == "application/json" || strings.HasSuffix(mediaType, "+json"):
		return &jsonCapture{}
	}
	return nil
}

// jsonCapture buffers a JSON body and parses it once it is complete.
type jsonCapture struct {
	buf      bytes.Buffer
	overflow bool
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
	if !c.overflow {
		d.mergeJSON(c.buf.Bytes())
	}
	return d
}

// sseCapture parses a text/event-stream body event by event, so memory use
// is bounded by the largest event rather than the whole stream. Metrics from
// later events override earlier ones.
type sseCapture struct {
	line     []byte // incomplete line carried over between writes
	data     []byte // data field of the event being assembled
	skipLine bool   // current line exceeds maxCaptureBytes and is discarded
	metrics  MetricsData
}

func (c *sseCapture) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			c.appendLine(p)
			break
		}
		c.appendLine(p[:i])
		if !c.skipLine {
			c.processLine(bytes.TrimSuffix(c.line, []byte("\r")))
		}
		c.line, c.skipLine = c.line[:0], false
		p = p[i+1:]
	}
	return n, nil
}

func (c *sseCapture) appendLine(p []byte) {
	if c.skipLine {
		return
	}
	if len(c.line)+len(p) > maxCaptureBytes {
		c.line, c.skipLine = c.line[:0], true
		return
	}
	c.line = append(c.line, p...)
}

func (c *sseCapture) processLine(line []byte) {
	switch {
	case len(line) == 0:
		c.dispatch()
	case bytes.HasPrefix(line, []byte("data:")):
		value := bytes.TrimPrefix(line[len("data:"):], []byte(" "))
		if len(c.data)+len(value) > maxCaptureBytes {
			c.data = c.data[:0]
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
		c.metrics.mergeJSON(c.data)
	}
	c.data = c.data[:0]
}

func (c *sseCapture) finish() MetricsData {
	if len(c.line) > 0 && !c.skipLine {
		c.processLine(bytes.TrimSuffix(c.line, []byte("\r")))
	}
	c.dispatch()
	return c.metrics
}

// llmPayload is the subset of upstream response fields used for metrics. It
// covers OpenAI-compatible responses (/v1/*) and LM Studio's native REST API
// v0 (/api/v0/*) and v1 (/api/v1/*), both as JSON bodies and as SSE events.
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
	// Wrapped payloads: the API v1 "chat.end" SSE event carries the response
	// in "result", OpenAI Responses API events in "response". Kept raw so an
	// unrelated field of the same name doesn't fail the whole parse.
	Result   json.RawMessage `json:"result"`
	Response json.RawMessage `json:"response"`
}

// mergeJSON applies the metrics found in a JSON document to d. Documents
// that are not JSON objects of the expected shape are ignored.
func (d *MetricsData) mergeJSON(raw []byte) {
	var p llmPayload
	if json.Unmarshal(raw, &p) == nil {
		d.merge(&p)
	}
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
	for _, raw := range []json.RawMessage{p.Result, p.Response} {
		var inner llmPayload
		if len(raw) > 0 && json.Unmarshal(raw, &inner) == nil {
			d.merge(&inner)
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
// response, or 0 if the response was not streamed.
func (d *MetricsData) completeDerived(streamTime time.Duration) {
	if d.TotalTokens == 0 {
		d.TotalTokens = d.PromptTokens + d.CompletionTokens
	}
	if d.TokensPerSecond > 0 || d.CompletionTokens == 0 {
		return
	}

	// Estimate generation time; for non-streamed responses without a
	// reported time to first token this includes prompt processing.
	genTime := d.Duration
	if d.TimeToFirstToken > 0 && d.TimeToFirstToken < genTime {
		genTime -= d.TimeToFirstToken
	} else if streamTime > 0 {
		genTime = streamTime
	}
	if genTime > 0 {
		d.TokensPerSecond = float64(d.CompletionTokens) / genTime.Seconds()
	}
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
func displayMetrics(info *requestInfo, status int, data MetricsData, snap metricsSnapshot, complete bool) {
	fmt.Fprintln(out, formatMetricsLine(time.Now(), info, status, data, snap, complete))
}

func formatMetricsLine(now time.Time, info *requestInfo, status int, data MetricsData, snap metricsSnapshot, complete bool) string {
	var b strings.Builder

	fmt.Fprintf(&b, "[%s] #%d %s %s %d",
		now.Format("15:04:05"), snap.QueryCount, info.method, info.path, status)

	if data.ModelName != "" {
		fmt.Fprintf(&b, " | Model: %s", truncateString(data.ModelName, 30))
	}
	if data.PromptTokens > 0 || data.CompletionTokens > 0 {
		fmt.Fprintf(&b, " | Tokens: %d in / %d out", data.PromptTokens, data.CompletionTokens)
	} else if data.TotalTokens > 0 {
		fmt.Fprintf(&b, " | Tokens: %d", data.TotalTokens)
	}
	if data.TokensPerSecond > 0 {
		fmt.Fprintf(&b, " | TPS: %.2f", data.TokensPerSecond)
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
