package main

import (
	"fmt"
	"io"
	"net/http"
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
	ProxyPort       int            `yaml:"proxy_port"`
	LmStudio        LmStudioConfig `yaml:"lm_studio"`
	APIEndpoints    []string       `yaml:"api_endpoints"`
	Metrics         MetricsConfig  `yaml:"metrics"`
	Logging         LoggingConfig  `yaml:"logging"`
}

type LmStudioConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

type MetricsConfig struct {
	Mode           string `yaml:"mode"`
	RefreshIntervalMs int   `yaml:"refresh_interval_ms"`
	HistoryPoints  int    `yaml:"history_points"`
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
	cfg       Config
	lmClient  *http.Client
	metrics   Metrics
)

func main() {
	fmt.Println("=== LM Studio Metrics Proxy ===")

	// Load configuration
	if err := loadConfig(); err != nil {
		fmt.Printf("Error loading config: %v\n", err)
		os.Exit(1)
	}

	// Initialize LM Studio client
	lmClient = &http.Client{
		Timeout: 30 * time.Second,
	}

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
		Addr:         fmt.Sprintf(":%d", cfg.ProxyPort),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	fmt.Println("Press Ctrl+C to stop")
	return server.ListenAndServe()
}

// isAPIEndpoint checks if the request path matches an API endpoint pattern
func isAPIEndpoint(path string) bool {
	for _, pattern := range cfg.APIEndpoints {
		if strings.HasPrefix(path, pattern) {
			return true
		}
	}
	return false
}

// forwardToLMStudio forwards the request to LM Studio and returns metrics
func forwardToLMStudio(req *http.Request, startTime time.Time) (*http.Response, MetricsData) {
	lmURL := fmt.Sprintf("http://%s:%s%s",
		cfg.LmStudio.Host,
		strconv.Itoa(cfg.LmStudio.Port),
		req.URL.Path)

	if req.URL.RawQuery != "" {
		lmURL += "?" + req.URL.RawQuery
	}

	fmt.Printf("[LM Studio] %s %s\n", req.Method, lmURL)

	resp, err := lmClient.Do(req)
	if err != nil {
		fmt.Printf("[ERROR] Failed to forward request: %v\n", err)
		return nil, MetricsData{}
	}

	// Parse response body for metrics
	bodyBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	metricsData := parseResponse(bodyBytes, startTime)

	fmt.Printf("[LM Studio] Response: %d tokens=%d tps=%.2f\n",
		resp.StatusCode, metricsData.TotalTokens, metricsData.TokensPerSecond)

	return resp, metricsData
}

// MetricsData holds parsed metrics from LM Studio response
type MetricsData struct {
	TokensPerSecond float64
	TotalTokens     int
	Duration        time.Duration
	ModelName       string
	QueryCount      int
}

// parseResponse extracts metrics from the response body
func parseResponse(body []byte, startTime time.Time) MetricsData {
	duration := time.Since(startTime)

	metrics := MetricsData{
		Duration: duration,
	}

	bodyStr := string(body)

	// Check for common LM Studio JSON response patterns
	if strings.Contains(bodyStr, `"total_tokens"`) ||
	strings.Contains(bodyStr, `"generated_tokens"`) {

		totalTokens := extractInt(bodyStr, "total_tokens")
		if totalTokens > 0 {
			metrics.TotalTokens = totalTokens
		}

		generatedTokens := extractInt(bodyStr, "generated_tokens")
		if generatedTokens > 0 && metrics.TotalTokens == 0 {
			metrics.TotalTokens = generatedTokens
		}

		tokensPerSec := extractFloat(bodyStr, "tokens_per_second")
		if tokensPerSec > 0 {
			metrics.TokensPerSecond = tokensPerSec
		}

		modelName := extractString(bodyStr, "model_name")
		if modelName != "" {
			metrics.ModelName = modelName
		}
	}

	// Update global metrics
	updateGlobalMetrics(startTime, metrics)

	return metrics
}

// updateGlobalMetrics updates the global metrics store
func updateGlobalMetrics(startTime time.Time, data MetricsData) {
	metrics.mu.Lock()
	defer metrics.mu.Unlock()

	metrics.QueryCount++

	// Calculate average time per query
	if metrics.QueryCount > 1 {
		totalDuration := metrics.AvgTimePerQuery * time.Duration(metrics.QueryCount-1) + time.Since(startTime)
		metrics.AvgTimePerQuery = totalDuration / time.Duration(metrics.QueryCount)
	} else {
		metrics.AvgTimePerQuery = time.Since(startTime)
	}

	// Add to history
	historyPoint := MetricsHistoryPoint{
		Timestamp:    time.Now(),
		TokensPerSec: data.TokensPerSecond,
		TotalTokens:  data.TotalTokens,
		Duration:     time.Since(startTime),
	}

	metrics.History = append(metrics.History, historyPoint)

	// Keep only the last N points
	if len(metrics.History) > cfg.Metrics.HistoryPoints {
		metrics.History = metrics.History[len(metrics.History)-cfg.Metrics.HistoryPoints:]
	}
}

// extractInt extracts an integer value from JSON-like string
func extractInt(s, key string) int {
	idx := strings.Index(s, `"`+key+`"`)
	if idx == -1 {
		return 0
	}

	start := idx + len(key) + 2 // skip "key":
	end := start

	for end < len(s) && (s[end] >= '0' && s[end] <= '9') {
		end++
	}

	if end > start {
		val, _ := strconv.Atoi(s[start:end])
		return val
	}

	return 0
}

// extractFloat extracts a float value from JSON-like string
func extractFloat(s, key string) float64 {
	idx := strings.Index(s, `"`+key+`"`)
	if idx == -1 {
		return 0
	}

	start := idx + len(key) + 2 // skip "key":
	end := start

	for end < len(s) && (s[end] >= '0' && s[end] <= '9') {
		end++
	}

	if end > start {
		val, _ := strconv.ParseFloat(s[start:end], 64)
		return val
	}

	return 0
}

// extractString extracts a string value from JSON-like string
func extractString(s, key string) string {
	idx := strings.Index(s, `"`+key+`"`)
	if idx == -1 {
		return ""
	}

	start := idx + len(key) + 2 // skip "key":
	end := start

	for end < len(s) && s[end] != '"' {
		if s[end] == '\\' && end+1 < len(s) {
			end += 2 // skip escaped character
		} else {
			end++
		}
	}

	if end > start {
		return s[start:end]
	}

	return ""
}

// handleRequest processes incoming requests
func handleRequest(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()

	path := r.URL.Path

	// Check if this is an API endpoint
	if !isAPIEndpoint(path) {
		fmt.Printf("[Proxy] Non-API request: %s %s\n", r.Method, path)
		http.NotFound(w, r)
		return
	}

	// Forward to LM Studio and collect metrics
	resp, _ := forwardToLMStudio(r, startTime)

	if resp == nil {
		fmt.Printf("[ERROR] Failed to get response from LM Studio\n")
		http.Error(w, "Failed to connect to LM Studio", http.StatusBadGateway)
		return
	}

	// Copy headers from LM Studio response
	for key, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// Stream response body
	io.Copy(w, resp.Body)
	resp.Body.Close()

	// Display metrics in terminal
	displayMetrics(startTime)
}

// displayMetrics shows collected metrics in the terminal
func displayMetrics(startTime time.Time) {
	metrics.mu.RLock()
	defer metrics.mu.RUnlock()

	fmt.Printf("\r[%s] Queries: %d | Model: %s",
		time.Now().Format("15:04:05"),
		metrics.QueryCount,
		truncateString(metrics.ModelName, 20))

	if metrics.QueryCount > 0 {
		fmt.Printf(" | Avg Time: %.2fms", float64(metrics.AvgTimePerQuery/time.Millisecond))

		if len(metrics.History) > 0 && metrics.History[0].TokensPerSec > 0 {
			fmt.Printf(" | TPS: %.2f", metrics.History[0].TokensPerSec)
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
