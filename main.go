package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	TokensPerSecond float64
	TotalTokens     int
	QueryCount      int
	ModelName       string
	AvgTimePerQuery time.Duration
	History         []MetricsHistoryPoint
	mu              sync.RWMutex
}

type MetricsHistoryPoint struct {
	Timestamp    time.Time
	TokensPerSec float64
	TotalTokens  int
	Duration     time.Duration
}

// Global config and metrics
var (
	cfg     Config
	lmProxy *httputil.ReverseProxy
	metrics Metrics
)

func main() {
	fmt.Println("=== LM Studio Metrics Proxy ===")

	// Load configuration
	if err := loadConfig(); err != nil {
		fmt.Printf("Error loading config: %v\n", err)
		os.Exit(1)
	}

	// Initialize reverse proxy to LM Studio
	lmProxy = newProxy(&url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(cfg.LmStudio.Host, strconv.Itoa(cfg.LmStudio.Port)),
	})

	// Setup signal handling for graceful shutdown
	go setupSignalHandling()

	// Start proxy server
	fmt.Printf("Proxy listening on port %d\n", cfg.ProxyPort)
	fmt.Printf("Forwarding to LM Studio at %s:%d\n", cfg.LmStudio.Host, cfg.LmStudio.Port)

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

	return yaml.Unmarshal(data, &cfg)
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
		// No read/write timeouts: generations can stream for minutes
		ReadHeaderTimeout: 10 * time.Second,
	}

	fmt.Println("Press Ctrl+C to stop")
	return server.ListenAndServe()
}

// isAPIEndpoint checks if the request path matches an API endpoint pattern.
// Patterns are path prefixes; a trailing "*" is optional ("/api/v1/*" == "/api/v1/").
func isAPIEndpoint(path string) bool {
	for _, pattern := range cfg.APIEndpoints {
		if strings.HasPrefix(path, strings.TrimSuffix(pattern, "*")) {
			return true
		}
	}
	return false
}

// startTimeKey is the request context key holding the time a request arrived
type startTimeKey struct{}

// newProxy creates a reverse proxy that forwards requests to LM Studio and
// collects metrics from the responses as they stream back to the client
func newProxy(target *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// Let the transport negotiate (and transparently decode) compression
			// so the metrics parser always sees plain text
			pr.Out.Header.Del("Accept-Encoding")
			fmt.Printf("[LM Studio] %s %s\n", pr.Out.Method, pr.Out.URL)
		},
		ModifyResponse: func(resp *http.Response) error {
			start, _ := resp.Request.Context().Value(startTimeKey{}).(time.Time)
			mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
			resp.Body = &meteredBody{
				ReadCloser: resp.Body,
				status:     resp.StatusCode,
				start:      start,
				parser: responseParser{
					streamed: mediaType == "text/event-stream" || mediaType == "application/x-ndjson",
				},
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			fmt.Printf("[ERROR] Failed to forward request: %v\n", err)
			http.Error(w, "Failed to connect to LM Studio", http.StatusBadGateway)
		},
	}
}

// meteredBody wraps a response body, feeding everything read through it to a
// parser and recording metrics once the body is fully consumed or closed
type meteredBody struct {
	io.ReadCloser
	status int
	start  time.Time
	parser responseParser
	once   sync.Once
}

func (b *meteredBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.parser.Write(p[:n])
	if err == io.EOF {
		b.finish()
	}
	return n, err
}

func (b *meteredBody) Close() error {
	err := b.ReadCloser.Close()
	b.finish()
	return err
}

func (b *meteredBody) finish() {
	b.once.Do(func() {
		data := b.parser.Result()
		data.Duration = time.Since(b.start)

		fmt.Printf("[LM Studio] Response: %d tokens=%d tps=%.2f\n",
			b.status, data.TotalTokens, data.TokensPerSecond)

		updateGlobalMetrics(data)
		displayMetrics()
	})
}

// MetricsData holds parsed metrics from LM Studio response
type MetricsData struct {
	TokensPerSecond float64
	TotalTokens     int
	Duration        time.Duration
	ModelName       string
	QueryCount      int
}

// maxParseBytes caps how much of a response (or of a single stream line) is
// buffered for metrics parsing; larger bodies are still proxied in full
const maxParseBytes = 4 << 20

// responseParser incrementally extracts metrics from a response body, which is
// either a single JSON document or a stream of SSE / NDJSON lines
type responseParser struct {
	streamed bool
	buf      []byte
	overflow bool
	data     MetricsData
}

// Write consumes the next chunk of the response body
func (p *responseParser) Write(b []byte) {
	if !p.streamed {
		p.buffer(b)
		return
	}
	for {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			p.buffer(b)
			return
		}
		p.buffer(b[:i])
		p.endLine()
		b = b[i+1:]
	}
}

// Result returns the metrics found in the body consumed so far
func (p *responseParser) Result() MetricsData {
	if p.streamed {
		p.endLine()
	} else if !p.overflow {
		p.parsePayload(p.buf)
	}
	return p.data
}

func (p *responseParser) buffer(b []byte) {
	if p.overflow {
		return
	}
	if len(p.buf)+len(b) > maxParseBytes {
		p.overflow = true
		p.buf = nil
		return
	}
	p.buf = append(p.buf, b...)
}

// endLine parses one complete stream line: an SSE "data:" field or an NDJSON object
func (p *responseParser) endLine() {
	if !p.overflow {
		line := bytes.TrimSpace(p.buf)
		if payload, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			p.parsePayload(bytes.TrimSpace(payload))
		} else if bytes.HasPrefix(line, []byte("{")) {
			p.parsePayload(line)
		}
	}
	p.buf = p.buf[:0]
	p.overflow = false
}

// lmPayload covers the response shapes of LM Studio's native API (/api/v1),
// Ollama's native API (/api/*) and their OpenAI-compatible APIs (/v1/*)
type lmPayload struct {
	Model           string `json:"model"`             // OpenAI, Ollama
	ModelInstanceID string `json:"model_instance_id"` // LM Studio
	Usage           *struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"` // OpenAI
	Stats *struct {
		InputTokens       int     `json:"input_tokens"`
		TotalOutputTokens int     `json:"total_output_tokens"`
		TokensPerSecond   float64 `json:"tokens_per_second"`
	} `json:"stats"` // LM Studio
	Timings *struct {
		PredictedPerSecond float64 `json:"predicted_per_second"`
	} `json:"timings"` // Ollama OpenAI-compatible
	PromptEvalCount int        `json:"prompt_eval_count"` // Ollama
	EvalCount       int        `json:"eval_count"`        // Ollama
	EvalDuration    int64      `json:"eval_duration"`     // Ollama, nanoseconds
	Result          *lmPayload `json:"result"`            // LM Studio "chat.end" stream event
}

// parsePayload merges metrics from one JSON document; later values override earlier ones
func (p *responseParser) parsePayload(raw []byte) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("[DONE]")) {
		return
	}
	var pl lmPayload
	// A type mismatch in one field still leaves the others decoded
	var typeErr *json.UnmarshalTypeError
	if err := json.Unmarshal(raw, &pl); err != nil && !errors.As(err, &typeErr) {
		return
	}
	p.apply(&pl)
}

func (p *responseParser) apply(pl *lmPayload) {
	if pl.ModelInstanceID != "" {
		p.data.ModelName = pl.ModelInstanceID
	} else if pl.Model != "" {
		p.data.ModelName = pl.Model
	}

	if pl.Usage != nil && pl.Usage.TotalTokens > 0 {
		p.data.TotalTokens = pl.Usage.TotalTokens
	}
	if pl.Stats != nil {
		if total := pl.Stats.InputTokens + pl.Stats.TotalOutputTokens; total > 0 {
			p.data.TotalTokens = total
		}
		if pl.Stats.TokensPerSecond > 0 {
			p.data.TokensPerSecond = pl.Stats.TokensPerSecond
		}
	}
	if pl.Timings != nil && pl.Timings.PredictedPerSecond > 0 {
		p.data.TokensPerSecond = pl.Timings.PredictedPerSecond
	}
	if total := pl.PromptEvalCount + pl.EvalCount; total > 0 {
		p.data.TotalTokens = total
	}
	if pl.EvalCount > 0 && pl.EvalDuration > 0 {
		p.data.TokensPerSecond = float64(pl.EvalCount) / time.Duration(pl.EvalDuration).Seconds()
	}

	if pl.Result != nil {
		p.apply(pl.Result)
	}
}

// updateGlobalMetrics updates the global metrics store
func updateGlobalMetrics(data MetricsData) {
	metrics.mu.Lock()
	defer metrics.mu.Unlock()

	metrics.QueryCount++
	metrics.TotalTokens += data.TotalTokens
	if data.TokensPerSecond > 0 {
		metrics.TokensPerSecond = data.TokensPerSecond
	}
	if data.ModelName != "" {
		metrics.ModelName = data.ModelName
	}

	// Running average time per query
	metrics.AvgTimePerQuery += (data.Duration - metrics.AvgTimePerQuery) / time.Duration(metrics.QueryCount)

	// Add to history
	metrics.History = append(metrics.History, MetricsHistoryPoint{
		Timestamp:    time.Now(),
		TokensPerSec: data.TokensPerSecond,
		TotalTokens:  data.TotalTokens,
		Duration:     data.Duration,
	})

	// Keep only the last N points
	if n := cfg.Metrics.HistoryPoints; n > 0 && len(metrics.History) > n {
		metrics.History = metrics.History[len(metrics.History)-n:]
	}
}

// handleRequest processes incoming requests
func handleRequest(w http.ResponseWriter, r *http.Request) {
	// Check if this is an API endpoint
	if !isAPIEndpoint(r.URL.Path) {
		fmt.Printf("[Proxy] Non-API request: %s %s\n", r.Method, r.URL.Path)
		http.NotFound(w, r)
		return
	}

	// Forward to LM Studio; metrics are collected as the response streams back
	ctx := context.WithValue(r.Context(), startTimeKey{}, time.Now())
	lmProxy.ServeHTTP(w, r.WithContext(ctx))
}

// displayMetrics shows collected metrics in the terminal
func displayMetrics() {
	metrics.mu.RLock()
	defer metrics.mu.RUnlock()

	fmt.Printf("\r[%s] Queries: %d | Model: %s",
		time.Now().Format("15:04:05"),
		metrics.QueryCount,
		truncateString(metrics.ModelName, 20))

	if metrics.QueryCount > 0 {
		fmt.Printf(" | Avg Time: %.2fms", float64(metrics.AvgTimePerQuery)/float64(time.Millisecond))

		if metrics.TokensPerSecond > 0 {
			fmt.Printf(" | TPS: %.2f", metrics.TokensPerSecond)
		}

		if metrics.TotalTokens > 0 {
			fmt.Printf(" | Tokens: %d", metrics.TotalTokens)
		}
	}

	fmt.Println()
}

// truncateString truncates a string to max length
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}
