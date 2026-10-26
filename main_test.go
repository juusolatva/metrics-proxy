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
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
)

// syncBuffer is a goroutine-safe buffer for capturing status output.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func resetMetrics() {
	metrics.mu.Lock()
	defer metrics.mu.Unlock()
	metrics.TokensPerSecond = 0
	metrics.TotalTokens = 0
	metrics.QueryCount = 0
	metrics.ModelName = ""
	metrics.AvgTimePerQuery = 0
	metrics.History = nil
}

// startUpstream starts a fake LM Studio server and returns its URL.
func startUpstream(t *testing.T, h http.Handler) *url.URL {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// startProxy resets global state and starts a proxy forwarding to target.
// It returns the proxy server and the buffer receiving its status output.
func startProxy(t *testing.T, target *url.URL) (*httptest.Server, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	out = logs
	cfg = Config{
		APIEndpoints: []string{"/v1/*", "/api/v1/*"},
		Metrics:      MetricsConfig{HistoryPoints: 10},
	}
	resetMetrics()
	csvLog = nil
	proxy = newReverseProxy(target)

	srv := httptest.NewServer(http.HandlerFunc(handleRequest))
	t.Cleanup(srv.Close)
	return srv, logs
}

// waitFor polls cond until it returns true or a deadline passes. Metrics are
// recorded on the proxy's goroutine, possibly just after the client has
// received the full body.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitForQueries(t *testing.T, n int) {
	t.Helper()
	waitFor(t, fmt.Sprintf("%d recorded queries", n), func() bool {
		metrics.mu.RLock()
		defer metrics.mu.RUnlock()
		return metrics.QueryCount >= n
	})
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const chatCompletionJSON = `{"id":"chatcmpl-1","object":"chat.completion","model":"qwen2.5-7b-instruct",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"Hi"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`

func TestIsAPIEndpoint(t *testing.T) {
	cfg = Config{APIEndpoints: []string{"/api/v1/*", "/v1/", "/exact"}}

	tests := []struct {
		path string
		want bool
	}{
		{"/api/v1/chat", true},
		{"/api/v1/", true},
		{"/api/v2/chat", false},
		{"/api", false},
		{"/v1/chat/completions", true},
		{"/exact", true},
		{"/exactly", true},
		{"/other", false},
	}
	for _, tt := range tests {
		if got := isAPIEndpoint(tt.path); got != tt.want {
			t.Errorf("isAPIEndpoint(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestParseConfigDefaults(t *testing.T) {
	for _, data := range []string{"", "# only a comment\n", "proxy_port: 0\napi_endpoints: []\n"} {
		c, err := parseConfig([]byte(data))
		if err != nil {
			t.Fatalf("%q: %v", data, err)
		}
		if c.ProxyPort != 11436 || !c.forwardsUnmatched() || c.Server.Host != "localhost" || c.Server.Port != 1234 || c.Metrics.HistoryPoints != 10 ||
			fmt.Sprint(c.APIEndpoints) != "[/api/v1/* /v1/*]" {
			t.Errorf("%q: defaults not applied: %+v", data, c)
		}
	}

	c, err := parseConfig([]byte("api_endpoints: [\"/api/*\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(c.APIEndpoints) != "[/api/*]" {
		t.Errorf("APIEndpoints = %q, want the configured list only", c.APIEndpoints)
	}

	for data, want := range map[string]bool{"forward_unmatched: false\n": false, "forward_unmatched: true\n": true} {
		c, err := parseConfig([]byte(data))
		if err != nil {
			t.Fatal(err)
		}
		if c.forwardsUnmatched() != want {
			t.Errorf("%q: forwardsUnmatched() = %t, want %t", data, !want, want)
		}
	}
}

func TestParseConfigForwardManagement(t *testing.T) {
	if c, err := parseConfig(nil); err != nil || c.ForwardManagement {
		t.Errorf("empty config: ForwardManagement = %t, %v; want false by default", c.ForwardManagement, err)
	}
	if c, err := parseConfig([]byte("forward_management: true\n")); err != nil || !c.ForwardManagement {
		t.Errorf("forward_management: true: ForwardManagement = %t, %v; want true", c.ForwardManagement, err)
	}
}

func TestParseConfigFile(t *testing.T) {
	data, err := os.ReadFile("config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := parseConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.APIEndpoints) == 0 {
		t.Error("config.yaml defines no api_endpoints")
	}
}

func TestParseConfigRejectsUnknownKeys(t *testing.T) {
	for _, tt := range []struct{ data, want string }{
		{"metrics:\n  histroy_points: 5\n", "line 2: field histroy_points not found"},
		{"proxy_port: 8080\nproxyport: 9090\n", "line 2: field proxyport not found"},
		{"metrics:\n  mode: \"periodic\"\n", "field mode not found"}, // removed setting
		{"upstream:\n  host: x\n", "field upstream not found"},
		{"lm_studio:\n  host: x\n", "field lm_studio not found"}, // renamed to server
	} {
		if _, err := parseConfig([]byte(tt.data)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%q: err = %v, want it to contain %q", tt.data, err, tt.want)
		}
	}
}

// TestParseConfigYAMLRules pins the YAML behavior parseConfig relies on from
// the pre-release go.yaml.in/yaml/v4, so that a dependency bump changing it
// fails here instead of changing how config files load.
func TestParseConfigYAMLRules(t *testing.T) {
	// YAML 1.1 booleans; explicit document start and end markers.
	for data, want := range map[string]bool{
		"forward_unmatched: no\n": false, "forward_unmatched: off\n": false,
		"forward_unmatched: yes\n": true, "---\nforward_unmatched: on\n...\n": true,
	} {
		c, err := parseConfig([]byte(data))
		if err != nil {
			t.Fatalf("%q: %v", data, err)
		}
		if c.forwardsUnmatched() != want {
			t.Errorf("%q: forwardsUnmatched() = %t, want %t", data, !want, want)
		}
	}

	for _, tt := range []struct{ data, want string }{
		{"proxy_port: 1\nproxy_port: 2\n", `line 2: mapping key "proxy_port" already defined at line 1`},
		{"server:\n  port: 1\n  port: 2\n", `line 3: mapping key "port" already defined at line 2`},
		{"proxy_port: 11436\n---\nserver:\n  port: 11434\n", "line 2: a second YAML document starts here"},
		{"proxy_port: 11436\n...\nserver: {}\n", "L3.C1"}, // content after the document end marker
		{"server:\n\thost: x\n", "L2.C1"},                 // tab indentation: syntax errors give L<line>.C<column>
		{"proxy_port: x\nlogging:\n  debug: maybe\n", "line 1: cannot construct !!str `x` into int; line 3:"},
	} {
		if _, err := parseConfig([]byte(tt.data)); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%q: err = %v, want it to contain %q", tt.data, err, tt.want)
		}
	}
}

func TestParseFlags(t *testing.T) {
	tests := []struct {
		args       []string
		wantConfig string
		wantCSV    string
		wantErr    string // substring of the printed output; "" means success
	}{
		{nil, "config.yaml", "", ""},
		{[]string{"--config", "/etc/metrics_proxy.yaml"}, "/etc/metrics_proxy.yaml", "", ""},
		{[]string{"--config=conf/ollama.yaml", "--csv", "stats.csv"}, "conf/ollama.yaml", "stats.csv", ""},
		{[]string{"-config", "x.yaml"}, "x.yaml", "", ""},
		{[]string{"--config"}, "", "", "flag needs an argument"},
		{[]string{"--config="}, "", "", "must not be empty"},
		{[]string{"ollama.yaml"}, "", "", `unexpected argument "ollama.yaml"`},
		{[]string{"--conf", "x.yaml"}, "", "", "flag provided but not defined"},
	}
	for _, tt := range tests {
		var output bytes.Buffer
		o, err := parseFlags(tt.args, &output)
		if tt.wantErr == "" {
			if err != nil || o.configPath != tt.wantConfig || o.csvPath != tt.wantCSV {
				t.Errorf("%q: got %+v, %v; want config %q, csv %q", tt.args, o, err, tt.wantConfig, tt.wantCSV)
			}
			continue
		}
		if err == nil || !strings.Contains(output.String(), tt.wantErr) {
			t.Errorf("%q: err %v, output %q; want an error mentioning %q", tt.args, err, output.String(), tt.wantErr)
		}
	}

	var output bytes.Buffer
	if _, err := parseFlags([]string{"--help"}, &output); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("--help: err = %v, want flag.ErrHelp", err)
	}
	if help := output.String(); !strings.Contains(help, "--config file") || !strings.Contains(help, `(default "config.yaml")`) || !strings.Contains(help, "--csv file") {
		t.Errorf("help output missing flags or default:\n%s", help)
	}
}

func TestLoadConfigFromPath(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "ollama.yaml")
	bad := filepath.Join(dir, "bad.yaml")
	os.WriteFile(good, []byte("server:\n  host: \"192.0.2.10\"\n  port: 11434\napi_endpoints: [\"/api/*\"]\n"), 0o644)
	os.WriteFile(bad, []byte("proxy_port: [not, a, port]\n"), 0o644)
	t.Cleanup(func() { cfg = Config{} })

	if err := loadConfig(good); err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Host != "192.0.2.10" || cfg.Server.Port != 11434 || cfg.ProxyPort != 11436 {
		t.Errorf("cfg = %+v, want upstream 192.0.2.10:11434 and default port", cfg)
	}

	for _, path := range []string{filepath.Join(dir, "missing.yaml"), bad, dir} {
		if err := loadConfig(path); err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("loadConfig(%q) = %v, want an error naming the file", path, err)
		}
	}
}

func TestProxyForwardsRequestAndResponse(t *testing.T) {
	reqBody := `{"model":"qwen2.5-7b-instruct","messages":[{"role":"user","content":"Hello"}]}`

	target := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || r.URL.RawQuery != "a=1" {
			t.Errorf("upstream got %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Errorf("upstream Authorization = %q", got)
		}
		if got := readAll(t, r.Body); got != reqBody {
			t.Errorf("upstream body = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream", "yes")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, chatCompletionJSON)
	}))
	srv, logs := startProxy(t, target)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions?a=1", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}
	if got := resp.Header.Get("X-Upstream"); got != "yes" {
		t.Errorf("X-Upstream header = %q", got)
	}
	if got := readAll(t, resp.Body); got != chatCompletionJSON {
		t.Errorf("client body = %q, want upstream body unchanged", got)
	}

	waitForQueries(t, 1)
	metrics.mu.RLock()
	defer metrics.mu.RUnlock()
	if metrics.ModelName != "qwen2.5-7b-instruct" {
		t.Errorf("ModelName = %q", metrics.ModelName)
	}
	if metrics.TotalTokens != 30 {
		t.Errorf("TotalTokens = %d, want 30", metrics.TotalTokens)
	}
	if metrics.TokensPerSecond <= 0 {
		t.Errorf("TokensPerSecond = %v, want derived rate > 0", metrics.TokensPerSecond)
	}
	if len(metrics.History) != 1 {
		t.Errorf("len(History) = %d, want 1", len(metrics.History))
	}
	if !strings.Contains(logs.String(), "Tokens: 10 in / 20 out") {
		t.Errorf("status line missing token counts:\n%s", logs)
	}
}

func TestProxyLMStudioNativeJSON(t *testing.T) {
	body := `{"model_instance_id":"openai/gpt-oss-20b","output":[{"type":"message","content":"Hi"}],` +
		`"stats":{"input_tokens":646,"total_output_tokens":586,"reasoning_output_tokens":0,` +
		`"tokens_per_second":29.753900615398926,"time_to_first_token_seconds":1.088},"response_id":"resp_1"}`

	target := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		io.WriteString(w, body)
	}))
	srv, logs := startProxy(t, target)

	resp, err := http.Post(srv.URL+"/api/v1/chat", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, resp.Body)
	resp.Body.Close()

	waitForQueries(t, 1)
	metrics.mu.RLock()
	defer metrics.mu.RUnlock()
	if metrics.ModelName != "openai/gpt-oss-20b" {
		t.Errorf("ModelName = %q", metrics.ModelName)
	}
	if metrics.TotalTokens != 646+586 {
		t.Errorf("TotalTokens = %d, want %d", metrics.TotalTokens, 646+586)
	}
	if metrics.TokensPerSecond != 29.753900615398926 {
		t.Errorf("TokensPerSecond = %v", metrics.TokensPerSecond)
	}
	if line := logs.String(); !strings.Contains(line, "TPS: 29.75") || !strings.Contains(line, "TTFT: 1.09s") {
		t.Errorf("status line missing reported stats:\n%s", line)
	}
}

func TestProxyStreamsSSEWithoutBuffering(t *testing.T) {
	first := "data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n"
	rest := "data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n" +
		"data: {\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n" +
		"data: [DONE]\n\n"

	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }

	target := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, first)
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, rest)
	}))
	srv, _ := startProxy(t, target)
	t.Cleanup(unblock) // runs before the servers close, so their handlers can finish

	// The headers and first event must arrive while the upstream is still
	// generating. A buffering proxy would block in http.Post itself, so the
	// request is made inside the timed goroutine, which also reads the rest
	// of the stream once released and closes the body.
	type result struct {
		body string
		err  error
	}
	gotFirst := make(chan result, 1)
	gotRest := make(chan result, 1)
	go func() {
		resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"stream":true}`))
		if err != nil {
			gotFirst <- result{err: err}
			return
		}
		defer resp.Body.Close()

		br := bufio.NewReader(resp.Body)
		var sb strings.Builder
		for !strings.HasSuffix(sb.String(), "\n\n") {
			line, err := br.ReadString('\n')
			sb.WriteString(line)
			if err != nil {
				break
			}
		}
		gotFirst <- result{body: sb.String()}

		b, err := io.ReadAll(br)
		gotRest <- result{body: string(b), err: err}
	}()

	select {
	case r := <-gotFirst:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.body != first {
			t.Fatalf("first event = %q, want %q", r.body, first)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first SSE event not delivered before upstream finished; response is being buffered")
	}

	unblock()
	select {
	case r := <-gotRest:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.body != rest {
			t.Errorf("remaining stream = %q, want %q", r.body, rest)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("rest of the stream not delivered after the upstream finished")
	}

	waitForQueries(t, 1)
	metrics.mu.RLock()
	defer metrics.mu.RUnlock()
	if metrics.ModelName != "m" || metrics.TotalTokens != 7 {
		t.Errorf("ModelName = %q, TotalTokens = %d; want m, 7", metrics.ModelName, metrics.TotalTokens)
	}
	if metrics.TokensPerSecond <= 0 {
		t.Errorf("TokensPerSecond = %v, want derived rate > 0", metrics.TokensPerSecond)
	}
}

const lmStudioV1Stream = "event: chat.start\r\ndata: {\"type\":\"chat.start\",\"model_instance_id\":\"openai/gpt-oss-20b\"}\r\n\r\n" +
	"event: message.delta\r\ndata: {\"type\":\"message.delta\",\"content\":\"Hi\"}\r\n\r\n" +
	"event: chat.end\r\ndata: {\"type\":\"chat.end\",\"result\":{\"model_instance_id\":\"openai/gpt-oss-20b\"," +
	"\"output\":[],\"stats\":{\"input_tokens\":329,\"total_output_tokens\":268,\"reasoning_output_tokens\":5," +
	"\"tokens_per_second\":43.73,\"time_to_first_token_seconds\":0.781},\"response_id\":\"resp_02\"}}\r\n\r\n"

func TestProxyLMStudioNativeSSE(t *testing.T) {
	target := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, lmStudioV1Stream)
	}))
	srv, _ := startProxy(t, target)

	resp, err := http.Post(srv.URL+"/api/v1/chat", "application/json", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, resp.Body); got != lmStudioV1Stream {
		t.Errorf("client stream altered: %q", got)
	}
	resp.Body.Close()

	waitForQueries(t, 1)
	metrics.mu.RLock()
	defer metrics.mu.RUnlock()
	if metrics.ModelName != "openai/gpt-oss-20b" || metrics.TotalTokens != 329+268 || metrics.TokensPerSecond != 43.73 {
		t.Errorf("got model %q, tokens %d, tps %v", metrics.ModelName, metrics.TotalTokens, metrics.TokensPerSecond)
	}
}

func TestSSECaptureSplitWrites(t *testing.T) {
	whole := &sseCapture{}
	whole.Write([]byte(lmStudioV1Stream))
	want := whole.finish()

	split := &sseCapture{}
	for i := range len(lmStudioV1Stream) {
		split.Write([]byte{lmStudioV1Stream[i]})
	}
	if got := split.finish(); got != want {
		t.Errorf("byte-by-byte = %+v, whole = %+v", got, want)
	}
	if want.TokensPerSecond != 43.73 || want.TimeToFirstToken != 781*time.Millisecond {
		t.Errorf("parsed %+v", want)
	}
}

func TestSSECaptureMultiLineDataAndUnterminatedEvent(t *testing.T) {
	c := &sseCapture{}
	// A JSON payload split over two data lines, then a final event without the
	// trailing blank line (stream cut off).
	c.Write([]byte("data: {\"model\":\"a\",\ndata: \"usage\":{\"total_tokens\":3}}\n\n: comment\n"))
	c.Write([]byte("data: {\"model\":\"b\"}"))
	got := c.finish()
	if got.ModelName != "b" || got.TotalTokens != 3 {
		t.Errorf("got %+v, want model b with 3 tokens", got)
	}
}

// readFixture returns a response captured from a real server in testdata/.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// Fixtures in testdata/ are real responses captured from LM Studio and Ollama.
func TestCaptureFixtures(t *testing.T) {
	const (
		lmModel = "qwen/qwen2.5-coder-14b"
		olModel = "qwen3:4b-instruct-2507-q8_0"
	)
	tests := []fixtureCase{
		{"lm_v1_chat.json", "application/json; charset=utf-8", lmModel, 41, 10, 80.17174569522265, 110923 * time.Microsecond},
		{"lm_v1_chat_stream.sse", "text/event-stream", lmModel, 41, 10, 84.25309630128908, 34515 * time.Microsecond},
		// LM Studio's OpenAI-compatible stats carry only speculative-decoding
		// counters (draft model), no rate.
		{"lm_oai.json", "application/json; charset=utf-8", lmModel, 41, 29, 0, 0},
		{"lm_oai_stream.sse", "text/event-stream", lmModel, 41, 17, 0, 0},
		{"lm_oai_stream_nousage.sse", "text/event-stream", lmModel, 0, 0, 0, 0},
		{"ol_chat.json", "application/json; charset=utf-8", olModel, 20, 10, 10 / 0.355075, 0},
		{"ol_chat_stream.ndjson", "application/x-ndjson", olModel, 20, 10, 10 / 0.341097, 0},
		{"ol_generate.json", "application/json; charset=utf-8", olModel, 20, 10, 10 / 0.340531, 0},
		{"ol_generate_stream.ndjson", "application/x-ndjson", olModel, 20, 10, 10 / 0.340514, 0},
		{"ol_oai.json", "application/json", olModel, 20, 10, 0, 0},
		{"ol_oai_stream.sse", "text/event-stream", olModel, 20, 10, 29.325513196480937, 0},
		{"ol_oai_stream_nousage.sse", "text/event-stream", olModel, 0, 0, 0, 0},
	}
	for _, tt := range tests {
		checkFixture(t, tt)
	}
}

// fixtureCase is a captured response in testdata/ and the metrics it holds.
type fixtureCase struct {
	file        string
	contentType string
	model       string
	in, out     int // prompt and completion tokens; 0 when not reported
	tps         float64
	ttft        time.Duration
}

// checkFixture parses a capture whole and byte by byte, to exercise chunk
// boundaries, and compares the result with the expected metrics.
func checkFixture(t *testing.T, tt fixtureCase) {
	t.Helper()
	body := readFixture(t, tt.file)
	for _, chunkSize := range []int{len(body), 1} {
		got, d := parseInChunks(t, tt.contentType, body, chunkSize)
		// Duration 0: totals are filled in but no throughput is estimated.
		got.completeDerived(0)
		want := MetricsData{
			ModelName: tt.model, PromptTokens: tt.in, CompletionTokens: tt.out, TotalTokens: tt.in + tt.out,
			TokensPerSecond: got.TokensPerSecond, TimeToFirstToken: tt.ttft,
		}
		if got != want || !approxEqual(got.TokensPerSecond, tt.tps) {
			t.Errorf("%s (chunk %d): got %+v, want model=%q tokens=%d+%d tps=%v ttft=%v",
				tt.file, chunkSize, got, tt.model, tt.in, tt.out, tt.tps, tt.ttft)
		}
		if d.invalid != 0 || d.oversized != 0 {
			t.Errorf("%s (chunk %d): diag %+v, want every document parsed", tt.file, chunkSize, d)
		}
	}
}

// parseInChunks writes body to a new capture for contentType in chunks of
// chunkSize bytes and returns what it found.
func parseInChunks(t *testing.T, contentType string, body []byte, chunkSize int) (MetricsData, captureDiag) {
	t.Helper()
	c := newCapture(contentType)
	if c == nil {
		t.Fatalf("no capture for %q", contentType)
	}
	for b := body; len(b) > 0; b = b[min(chunkSize, len(b)):] {
		c.Write(b[:min(chunkSize, len(b))])
	}
	return c.finish(), c.diag()
}

func TestCaptureEdgeCases(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		want        MetricsData
	}{
		{"non-JSON body", "application/json", "Not Found", MetricsData{}},
		{"truncated JSON", "application/json", `{"model":"m","usage":{"total_to`, MetricsData{}},
		{"field type mismatch keeps other fields", "application/json",
			`{"model":{"unexpected":"object"},"usage":{"total_tokens":7}}`,
			MetricsData{TotalTokens: 7}},
		{"Ollama generate response text is not a wrapped payload", "application/json",
			`{"model":"m","response":"hi","prompt_eval_count":1,"eval_count":2}`,
			MetricsData{ModelName: "m", PromptTokens: 1, CompletionTokens: 2}},
		{"oversized body is not parsed", "application/json",
			`{"model":"m","pad":"` + strings.Repeat("x", maxCaptureBytes) + `"}`,
			MetricsData{}},
		{"CRLF SSE without trailing newline", "text/event-stream",
			"event: chat.end\r\ndata: {\"result\":{\"model_instance_id\":\"m\",\"stats\":{\"tokens_per_second\":3.5}}}",
			MetricsData{ModelName: "m", TokensPerSecond: 3.5}},
		{"oversized SSE line is skipped", "text/event-stream",
			"data: {\"pad\":\"" + strings.Repeat("x", maxCaptureBytes) + "\"}\n\n" +
				"data: {\"model\":\"m\",\"usage\":{\"total_tokens\":4}}\n\n",
			MetricsData{ModelName: "m", TotalTokens: 4}},
		{"NDJSON with CRLF, blank and unterminated lines", "application/x-ndjson",
			"{\"model\":\"a\"}\r\n\r\n{\"model\":\"b\",\"eval_count\":4,\"eval_duration\":2000000000}",
			MetricsData{ModelName: "b", CompletionTokens: 4, TokensPerSecond: 2}},
		{"oversized NDJSON line is skipped", "application/x-ndjson",
			"{\"pad\":\"" + strings.Repeat("x", maxCaptureBytes) + "\"}\n{\"model\":\"m\"}\n",
			MetricsData{ModelName: "m"}},
	}
	for _, tt := range tests {
		c := newCapture(tt.contentType)
		c.Write([]byte(tt.body))
		if got := c.finish(); got != tt.want {
			t.Errorf("%s: got %+v, want %+v", tt.name, got, tt.want)
		}
	}
}

// Bodies arriving in chunks cross the size limit partway through. The rest of
// an oversized JSON body or SSE event is dropped, including SSE data lines
// that follow the oversized part (they would be parsed as a fragment), and
// later events still count.
func TestCaptureOversizedInChunks(t *testing.T) {
	half := strings.Repeat("x", maxCaptureBytes/2+1)
	fragment := "data: {\"model\":\"fragment\"}\n"
	next := "data: {\"model\":\"m\",\"usage\":{\"total_tokens\":4}}\n\n"
	for _, tt := range []struct {
		name, contentType, body string
		want                    MetricsData
		documents               int
	}{
		{"JSON body", "application/json", `{"model":"m","pad":"` + half + half + half + `"}`, MetricsData{}, 0}, // writes continue after the overflow
		{"SSE data lines of one event", "text/event-stream",
			"data: " + half + "\ndata: " + half + "\n" + fragment + "\n" + next, MetricsData{ModelName: "m", TotalTokens: 4}, 1},
		{"SSE oversized line within an event", "text/event-stream",
			"event: x\ndata: " + half + half + "\n" + fragment + "\n" + next, MetricsData{ModelName: "m", TotalTokens: 4}, 1},
	} {
		got, d := parseInChunks(t, tt.contentType, []byte(tt.body), 1<<20)
		if got != tt.want || d.oversized != 1 || d.documents != tt.documents || d.invalid != 0 {
			t.Errorf("%s: got %+v, diag %+v; want %+v, 1 oversized, %d document(s)", tt.name, got, d, tt.want, tt.documents)
		}
	}
}

// Synthetic wrappers exercise hostile inputs not present in the real captures.
func wrappedMetrics(payload string, depth int) string {
	for i := range depth {
		key := "result"
		if i%2 != 0 {
			key = "response"
		}
		payload = `{"` + key + `":` + payload + `}`
	}
	return payload
}

func metricsBody(contentType, payload string) []byte {
	if contentType == "text/event-stream" {
		return []byte("data: " + payload + "\n\n")
	}
	return []byte(payload + "\n")
}

func TestCaptureWrapperDepthLimit(t *testing.T) {
	for _, contentType := range []string{"application/json", "text/event-stream", "application/x-ndjson"} {
		t.Run(contentType, func(t *testing.T) { checkWrapperDepthLimit(t, contentType) })
	}
}

func checkWrapperDepthLimit(t *testing.T, contentType string) {
	t.Helper()
	leaf := `{"model":"inner","pad":"` + strings.Repeat("x", 16<<10) + `"}`
	for _, depth := range []int{8, 9, 64} {
		payload := wrappedMetrics(leaf, depth)
		// The root's own fields remain useful when deeper inspection stops.
		payload = `{"model":"outer",` + payload[1:]
		got, diag := parseInChunks(t, contentType, metricsBody(contentType, payload), 1024)
		want := "inner"
		if depth > 8 {
			want = "outer"
		}
		if got.ModelName != want || diag.documents != 1 || diag.invalid != 0 {
			t.Errorf("depth %d: got %+v, diag %+v; want model %q and one valid document", depth, got, diag, want)
		}
	}
}

func TestCaptureWrapperBudgetSharedBySiblings(t *testing.T) {
	// Root ~12 MiB + three ~6 MiB result decodes leaves too little of the
	// 32 MiB work budget for the response sibling. Neither branch hits depth 8.
	pad := strings.Repeat("x", 6<<20)
	first := wrappedMetrics(`{"model":"first","pad":"`+pad+`"}`, 2)
	payload := `{"model":"outer","result":` + first + `,"response":{"model":"second","pad":"` + pad + `"}}`
	for _, contentType := range []string{"application/json", "text/event-stream", "application/x-ndjson"} {
		got, diag := parseInChunks(t, contentType, metricsBody(contentType, payload), 32<<10)
		if got.ModelName != "first" || diag.documents != 1 || diag.oversized != 0 {
			t.Errorf("%s: got %+v, diag %+v; want first and no capture overflow", contentType, got, diag)
		}
	}
}

func TestWrapperParseBudgetBoundary(t *testing.T) {
	raw := []byte(`{"model":"outer","result":{"model":"first"},"response":{"model":"second"}}`)
	for _, extra := range []int{-1, 0, len(`{"model":"first"}`), len(`{"model":"first"}`) + len(`{"model":"second"}`)} {
		remaining := len(raw) + extra
		var got MetricsData
		ok := got.mergeJSONBounded(raw, 0, &remaining)
		want := map[int]string{-1: "", 0: "outer", len(`{"model":"first"}`): "first"}[extra]
		if extra == len(`{"model":"first"}`)+len(`{"model":"second"}`) {
			want = "second"
		}
		if ok != (extra >= 0) || got.ModelName != want || remaining < 0 {
			t.Errorf("extra %d: model %q, valid %t, remaining %d; want %q", extra, got.ModelName, ok, remaining, want)
		}
	}
}

func TestProxyForwardsBeyondWrapperLimit(t *testing.T) {
	for _, contentType := range []string{"application/json", "text/event-stream", "application/x-ndjson"} {
		t.Run(contentType, func(t *testing.T) { checkProxyWrapperLimit(t, contentType) })
	}
}

func checkProxyWrapperLimit(t *testing.T, contentType string) {
	t.Helper()
	payload := `{"model":"outer",` + wrappedMetrics(`{"model":"too-deep"}`, 64)[1:]
	body := metricsBody(contentType, payload)
	if contentType != "application/json" {
		body = append(body, metricsBody(contentType, `{"response":{"model":"next"}}`)...)
	}
	target := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Write(body)
	}))
	srv, _ := startProxy(t, target)
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if readAll(t, resp.Body) != string(body) {
		t.Fatal("inspection changed the forwarded response")
	}
	waitForQueries(t, 1)
	want := "outer"
	if contentType != "application/json" {
		want = "next" // later documents get a fresh inspection budget
	}
	metrics.mu.RLock()
	defer metrics.mu.RUnlock()
	if metrics.ModelName != want {
		t.Errorf("model = %q, want %q", metrics.ModelName, want)
	}
}

func TestProxyOllamaNativeNDJSON(t *testing.T) {
	stream := readFixture(t, "ol_chat_stream.ndjson")
	target := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Write(stream)
	}))
	srv, _ := startProxy(t, target)
	cfg.APIEndpoints = append(cfg.APIEndpoints, "/api/*")

	resp, err := http.Post(srv.URL+"/api/chat", "application/json", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, resp.Body); got != string(stream) {
		t.Errorf("client stream altered: %q", got)
	}
	resp.Body.Close()

	waitForQueries(t, 1)
	metrics.mu.RLock()
	defer metrics.mu.RUnlock()
	if metrics.ModelName != "qwen3:4b-instruct-2507-q8_0" || metrics.TotalTokens != 30 || !approxEqual(metrics.TokensPerSecond, 10/0.341097) {
		t.Errorf("got model %q, tokens %d, tps %v", metrics.ModelName, metrics.TotalTokens, metrics.TokensPerSecond)
	}
}

func approxEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-6
}

func TestUpstreamUnavailable(t *testing.T) {
	up := httptest.NewServer(http.NotFoundHandler())
	target, _ := url.Parse(up.URL)
	up.Close()
	srv, logs := startProxy(t, target)

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadGateway)
	}
	if !strings.Contains(logs.String(), "[ERROR]") {
		t.Errorf("no error logged:\n%s", logs)
	}
}

// servedProxy is the proxy run through serve, as main runs it.
type servedProxy struct {
	url, addr   string
	logs        *syncBuffer
	stop, force context.CancelFunc
	done        chan struct{} // closed when serve returns
	err         error         // serve's result, set before done is closed
}

// wait returns serve's result, failing the test if it doesn't return in time.
func (p *servedProxy) wait(t *testing.T, timeout time.Duration) error {
	t.Helper()
	select {
	case <-p.done:
		return p.err
	case <-time.After(timeout):
		t.Fatal("serve did not return")
		return nil
	}
}

func startServe(t *testing.T, target *url.URL, timeout time.Duration) *servedProxy {
	t.Helper()
	_, logs := startProxy(t, target)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stopCtx, stop := context.WithCancel(context.Background())
	forceCtx, force := context.WithCancel(context.Background())
	p := &servedProxy{
		url: "http://" + ln.Addr().String(), addr: ln.Addr().String(),
		logs: logs, stop: stop, force: force, done: make(chan struct{}),
	}
	go func() {
		defer close(p.done)
		p.err = serve(stopCtx, forceCtx, &http.Server{Handler: http.HandlerFunc(handleRequest)}, ln, timeout)
	}()
	t.Cleanup(func() {
		stop()
		force()
		p.wait(t, 5*time.Second)
	})
	return p
}

// startStreamingUpstream serves an SSE response whose first event is sent
// immediately and whose rest is sent once release is closed.
func startStreamingUpstream(t *testing.T, first, rest string) (target *url.URL, release func()) {
	t.Helper()
	ch := make(chan struct{})
	var once sync.Once
	release = func() { once.Do(func() { close(ch) }) }
	target = startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, first)
		w.(http.Flusher).Flush()
		select {
		case <-ch:
			io.WriteString(w, rest)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(release) // runs before the upstream closes
	return target, release
}

// postStream sends a streaming request and reads the first SSE event.
func postStream(t *testing.T, base string) (*http.Response, *bufio.Reader) {
	t.Helper()
	resp, err := http.Post(base+"/v1/chat/completions", "application/json", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	br := bufio.NewReader(resp.Body)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading first event: %v", err)
		}
		if line == "\n" {
			return resp, br
		}
	}
}

const (
	streamFirst = "data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n"
	streamRest  = "data: {\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n" +
		"data: [DONE]\n\n"
)

func TestGracefulShutdownWaitsForInFlightStream(t *testing.T) {
	target, release := startStreamingUpstream(t, streamFirst, streamRest)
	p := startServe(t, target, time.Minute)
	_, br := postStream(t, p.url)

	p.stop()
	waitFor(t, "listener to close", func() bool {
		conn, err := net.Dial("tcp", p.addr)
		if err == nil {
			conn.Close()
		}
		return err != nil
	})
	if line := p.logs.String(); !strings.Contains(line, "waiting up to 1m0s for 1 in-flight request(s)") {
		t.Errorf("shutdown message missing from logs:\n%s", line)
	}
	select {
	case <-p.done:
		t.Fatalf("serve returned (%v) while a stream was in flight", p.err)
	case <-time.After(50 * time.Millisecond):
	}

	release()
	if got := readAll(t, br); got != streamRest {
		t.Errorf("remaining stream = %q, want %q", got, streamRest)
	}
	if err := p.wait(t, 2*time.Second); err != nil {
		t.Errorf("serve() = %v, want nil", err)
	}

	logs := p.logs.String()
	if !strings.Contains(logs, "Total tokens: 7") || strings.Contains(logs, "incomplete") || !strings.HasSuffix(logs, "Proxy stopped\n") {
		t.Errorf("want one complete query and a stop message, logs:\n%s", logs)
	}
}

func TestShutdownAbortsInFlightStream(t *testing.T) {
	for _, tt := range []struct {
		name    string
		timeout time.Duration
		force   bool
		want    string
	}{
		{"timeout", 100 * time.Millisecond, false, "Shutdown timed out: closing 1 request(s)"},
		{"second signal", time.Minute, true, "Shutdown forced: closing 1 request(s)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			target, _ := startStreamingUpstream(t, streamFirst, streamRest)
			p := startServe(t, target, tt.timeout)
			_, br := postStream(t, p.url)

			p.stop()
			if tt.force {
				waitFor(t, "shutdown to start", func() bool { return strings.Contains(p.logs.String(), "waiting up to") })
				p.force()
			}
			if err := p.wait(t, 2*time.Second); err != nil {
				t.Errorf("serve() = %v, want nil", err)
			}
			if _, err := io.ReadAll(br); err == nil {
				t.Error("client read the stream to a clean EOF; want it cut off")
			}

			logs := p.logs.String()
			if !strings.Contains(logs, tt.want) || !strings.Contains(logs, "| incomplete") || !strings.HasSuffix(logs, "Proxy stopped\n") {
				t.Errorf("want %q, an incomplete query and a stop message, logs:\n%s", tt.want, logs)
			}
		})
	}
}

// A client that disconnects mid-stream cancels the upstream request. The
// query is recorded as incomplete with what arrived until then.
func TestClientDisconnectMidStream(t *testing.T) {
	target, _ := startStreamingUpstream(t, streamFirst, streamRest)
	srv, logs := startProxy(t, target)
	cfg.Logging.Debug = true

	resp, _ := postStream(t, srv.URL)
	resp.Body.Close()
	waitFor(t, "incomplete query", func() bool { return strings.Contains(logs.String(), "| incomplete") })
	for _, want := range []string{"body ended before EOF (client disconnected or shutdown)", "| Model: m |"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs missing %q:\n%s", want, logs)
		}
	}
}

func TestShutdownWithoutInFlightRequests(t *testing.T) {
	p := startServe(t, startUpstream(t, http.NotFoundHandler()), time.Minute)
	p.stop()
	if err := p.wait(t, 2*time.Second); err != nil {
		t.Errorf("serve() = %v, want nil", err)
	}
	if logs := p.logs.String(); logs != "\nShutting down\nProxy stopped\n" {
		t.Errorf("logs = %q, want only the shutdown and stop messages", logs)
	}
}

func TestServeReturnsListenerError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	ctx := context.Background()
	if err := serve(ctx, ctx, &http.Server{Handler: http.NotFoundHandler()}, ln, time.Second); !errors.Is(err, net.ErrClosed) {
		t.Errorf("serve() on a closed listener = %v, want net.ErrClosed", err)
	}
}

func TestRunProxyPortInUse(t *testing.T) {
	startProxy(t, startUpstream(t, http.NotFoundHandler()))
	busy, err := net.Listen("tcp", ":0") // same wildcard address runProxy listens on
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	cfg.ProxyPort = busy.Addr().(*net.TCPAddr).Port

	ctx := context.Background()
	if err := runProxy(ctx, ctx); err == nil {
		t.Errorf("runProxy() on port %d in use: want an error", cfg.ProxyPort)
	}
}

func TestRunProxyServesUntilStopped(t *testing.T) {
	target, hits := startCountingUpstream(t)
	startProxy(t, target)
	free, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.ProxyPort = free.Addr().(*net.TCPAddr).Port
	free.Close()

	stopCtx, stop := context.WithCancel(context.Background())
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- runProxy(stopCtx, context.Background()) }()

	url := fmt.Sprintf("http://127.0.0.1:%d/v1/models", cfg.ProxyPort)
	waitFor(t, "proxy to answer", func() bool {
		resp, err := http.Get(url)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	stop()
	select {
	case err := <-errc:
		if err != nil {
			t.Errorf("runProxy() = %v, want nil after stop", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runProxy() did not return after stop")
	}
	if hits.Load() == 0 {
		t.Error("no request reached the upstream")
	}
}

func TestUpstreamURL(t *testing.T) {
	for host, want := range map[string]string{
		"localhost": "http://localhost:1234", "192.0.2.1": "http://192.0.2.1:1234", "::1": "http://[::1]:1234",
	} {
		cfg.Server = ServerConfig{Host: host, Port: 1234}
		if got := upstreamURL().String(); got != want {
			t.Errorf("upstreamURL() for host %q = %q, want %q", host, got, want)
		}
	}
}

func TestNonAPIPathForwardedUnmetered(t *testing.T) {
	target := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"models":[{"name":"qwen3:4b","model":"qwen3:4b"}]}`)
	}))
	srv, logs := startProxy(t, target)

	for path, wantStatus := range map[string]int{"/api/tags": http.StatusOK, "/models": http.StatusNotFound} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != wantStatus {
			t.Errorf("%s: status = %d, want the upstream's %d", path, resp.StatusCode, wantStatus)
		}
		if path == "/api/tags" && !strings.Contains(string(body), "qwen3:4b") {
			t.Errorf("%s: body = %q, want the upstream's", path, body)
		}
		if want := fmt.Sprintf("[Proxy] GET %s %d (", path, wantStatus); !strings.Contains(logs.String(), want) {
			t.Errorf("logs missing %q:\n%s", want, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "not metered)") {
		t.Errorf("logs don't mark the requests as not metered:\n%s", logs.String())
	}
	metrics.mu.RLock()
	defer metrics.mu.RUnlock()
	if metrics.QueryCount != 0 {
		t.Errorf("QueryCount = %d, want unmetered requests not counted", metrics.QueryCount)
	}
}

func TestNonAPIPathNotForwarded(t *testing.T) {
	var hits atomic.Int32
	target := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	srv, _ := startProxy(t, target)
	forward := false
	cfg.ForwardUnmatched = &forward

	resp, err := http.Get(srv.URL + "/admin")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	if hits.Load() != 0 {
		t.Error("non-API request was forwarded upstream")
	}
}

func TestIsManagementEndpoint(t *testing.T) {
	for p, want := range map[string]bool{
		"/api/pull": true, "/api/pull/": true, "/API/Pull": true, "//api/pull": true, "/api/../api/delete": true,
		"/v1/../api/push": true, `/api\pull`: true, `\api\pull`: true, `/api/v1/models\load`: true, "/api/copy": true, "/api/create": true, "/api/blobs/sha256:0": true,
		"/api/me": true, "/api/signout": true, "/api/user/keys/x": true,
		"/api/v1/models/load": true, "/api/v1/models/unload": true, "/api/v1/models/download/status/1": true,
		"/api/chat": false, "/api/generate": false, "/api/embed": false, "/api/tags": false, "/api/show": false,
		"/api/ps": false, "/api/version": false, "/api/pullx": false, "/api/web_search": false, "/api/web_fetch": false,
		"/api/v1/models": false, "/api/v1/chat": false, "/v1/models": false, "/v1/chat/completions": false,
		"/": false, "*": false, "": false,
	} {
		if got := isManagementEndpoint(p); got != want {
			t.Errorf("isManagementEndpoint(%q) = %t, want %t", p, got, want)
		}
	}
}

func TestHasDotDotSegment(t *testing.T) {
	for p, want := range map[string]bool{
		"/api/pull/..": true, "/api/../api/pull": true, "..": true, "/api/blobs/..": true, "/v1/x/../../api/pull": true,
		`/api/pull\..`: true, `/api/x\..\pull`: true, `\..\api`: true,
		"/api/pull": false, "/api/pull/": false, "/v1/chat/completions": false, "/": false, "": false,
		"/api/./pull": false, "/v1/models/a..b": false, "/v1/.../x": false, "/v1/..x": false, "/v1/x..": false,
	} {
		if got := hasDotDotSegment(p); got != want {
			t.Errorf("hasDotDotSegment(%q) = %t, want %t", p, got, want)
		}
	}
}

// Paths with ".." segments are checked after path.Clean but forwarded as
// sent, so they are refused before the management check, in every
// configuration ("/api/blobs/.." would otherwise be checked as "/api").
func TestDotDotPathNotForwarded(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		forwardManagement, forward bool
	}{
		{"default", false, true},
		{"forward_management", true, true},
		{"forward_unmatched false", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkDotDotRefused(t, tc.forwardManagement, tc.forward)
		})
	}
}

func checkDotDotRefused(t *testing.T, forwardManagement, forwardUnmatched bool) {
	target, hits := startCountingUpstream(t)
	srv, logs := startProxy(t, target)
	cfg.APIEndpoints = []string{"/api/*", "/v1/*"}
	cfg.ForwardManagement = forwardManagement
	cfg.ForwardUnmatched = &forwardUnmatched

	for _, p := range []string{
		"/api/pull/..", "/api/pull/%2e%2e", "/api/pull/.%2E", "/api/pull%2F..", "/api/blobs/..",
		"/api/delete/x/../..", "/api/../api/pull", "/v1/../api/push", "/api/x%5C..%5Cpull", "/v1/../secret",
	} {
		if resp := sendRequest(t, "POST", srv.URL+p); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("POST %s: status = %d, want 400", p, resp.StatusCode)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("%d request(s) with \"..\" segments reached the upstream", n)
	}
	if want := `[Proxy] Rejected POST /api/pull/..: ".." segment in path`; !strings.Contains(logs.String(), want) {
		t.Errorf("logs missing %q:\n%s", want, logs)
	}

	// Dots inside a segment are ordinary characters.
	if resp := sendRequest(t, "GET", srv.URL+"/v1/models/a..b"); resp.StatusCode != http.StatusOK {
		t.Errorf("GET /v1/models/a..b: status = %d, want the upstream's 200", resp.StatusCode)
	}
}

// startCountingUpstream starts a fake upstream that counts the requests
// reaching it. It accepts WebSocket handshakes, as LM Studio's SDK API does,
// and echoes the first 4 bytes sent over the upgraded connection. Other
// requests get a JSON body and an X-Upgrade-Seen header with the Upgrade
// header they arrived with.
func startCountingUpstream(t *testing.T) (*url.URL, *atomic.Int32) {
	t.Helper()
	hits := &atomic.Int32{}
	target := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			echoUpgraded(t, w)
			return
		}
		w.Header().Set("X-Upgrade-Seen", r.Header.Get("Upgrade"))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"success"}`)
	}))
	return target, hits
}

// echoUpgraded switches the connection to a WebSocket and echoes 4 bytes.
func echoUpgraded(t *testing.T, w http.ResponseWriter) {
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Error(err)
		return
	}
	defer conn.Close()
	brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	brw.Flush()
	buf := make([]byte, 4)
	if _, err := io.ReadFull(brw, buf); err == nil {
		brw.Write(buf)
		brw.Flush()
	}
}

// sendRequest sends a request with the given header name/value pairs and
// returns the response, with its body read and closed. The body of a 101
// (an upgraded connection, which doesn't end) is closed unread.
func sendRequest(t *testing.T, method, url string, header ...string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		io.Copy(io.Discard, resp.Body)
	}
	resp.Body.Close()
	return resp
}

// webSocketHandshake holds the headers of a WebSocket opening handshake.
var webSocketHandshake = []string{
	"Connection", "Upgrade", "Upgrade", "websocket",
	"Sec-WebSocket-Version", "13", "Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==", // betterleaks:allow (RFC 6455 sample nonce, not a secret)
}

func TestManagementEndpointNotForwarded(t *testing.T) {
	target, hits := startCountingUpstream(t)
	srv, logs := startProxy(t, target)
	cfg.APIEndpoints = []string{"/api/*", "/v1/*"} // documented Ollama setup; "/api/*" matches the management paths too

	for _, rq := range [][2]string{
		{"DELETE", "/api/delete"}, {"POST", "/api/pull"}, {"POST", "/api/push"}, {"POST", "/api/copy"},
		{"POST", "/api/create"}, {"HEAD", "/api/blobs/sha256:0"}, {"POST", "/api/me"}, {"POST", "/api/signout"},
		{"POST", "/api/%70ull"}, {"POST", "/api%5Cpull"}, {"POST", "//api/pull"}, {"POST", "/API/PULL"},
		{"POST", "/api/pull/"}, {"POST", "/api/v1/models/load"}, {"POST", "/api/v1/models/download"},
	} {
		if resp := sendRequest(t, rq[0], srv.URL+rq[1]); resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s: status = %d, want 403", rq[0], rq[1], resp.StatusCode)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("%d management request(s) reached the upstream", n)
	}
	if want := "[Proxy] Management request not forwarded: DELETE /api/delete (forward_management: false)"; !strings.Contains(logs.String(), want) {
		t.Errorf("logs missing %q:\n%s", want, logs)
	}

	// Queries and listings are still forwarded.
	for _, rq := range [][2]string{
		{"POST", "/api/chat"}, {"GET", "/api/tags"}, {"POST", "/api/show"}, {"GET", "/api/version"},
		{"GET", "/api/v1/models"}, {"GET", "/v1/models"},
	} {
		if resp := sendRequest(t, rq[0], srv.URL+rq[1]); resp.StatusCode != http.StatusOK {
			t.Errorf("%s %s: status = %d, want the upstream's 200", rq[0], rq[1], resp.StatusCode)
		}
	}
	if n := hits.Load(); n != 6 {
		t.Errorf("upstream got %d requests, want 6", n)
	}
}

func TestUpgradeNotForwarded(t *testing.T) {
	target, hits := startCountingUpstream(t)
	srv, logs := startProxy(t, target) // forward_unmatched unset (true), as by default

	// LM Studio SDK namespaces and a metered path.
	for _, p := range []string{"/llm", "/repository", "/system", "/v1/chat/completions"} {
		if resp := sendRequest(t, http.MethodGet, srv.URL+p, webSocketHandshake...); resp.StatusCode != http.StatusForbidden {
			t.Errorf("WebSocket handshake to %s: status = %d, want 403", p, resp.StatusCode)
		}
	}
	for _, upgrade := range []string{"WebSocket", "h2c, websocket"} {
		header := append(append([]string{}, webSocketHandshake...), "Upgrade", upgrade, "Connection", "keep-alive, Upgrade")
		if resp := sendRequest(t, http.MethodGet, srv.URL+"/llm", header...); resp.StatusCode != http.StatusForbidden {
			t.Errorf("Upgrade %q: status = %d, want 403", upgrade, resp.StatusCode)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("%d upgrade request(s) reached the upstream", n)
	}
	if want := "[Proxy] Protocol upgrade not forwarded: GET /llm (forward_management: false)"; !strings.Contains(logs.String(), want) {
		t.Errorf("logs missing %q:\n%s", want, logs)
	}

	// An h2c offer (Java's HttpClient sends one with ordinary requests) is
	// dropped and the request forwarded as plain HTTP/1.1.
	resp := sendRequest(t, http.MethodGet, srv.URL+"/v1/models",
		"Connection", "Upgrade, HTTP2-Settings", "Upgrade", "h2c", "HTTP2-Settings", "AAMAAABkAARAAAAAAAIAAAAA")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Upgrade-Seen") != "" {
		t.Errorf("h2c offer: status = %d, upstream saw Upgrade %q; want 200 and no Upgrade", resp.StatusCode, resp.Header.Get("X-Upgrade-Seen"))
	}
}

func TestManagementForwardedWhenEnabled(t *testing.T) {
	target, hits := startCountingUpstream(t)
	srv, logs := startProxy(t, target)
	cfg.APIEndpoints = []string{"/api/*", "/v1/*"}
	cfg.ForwardManagement = true

	for _, p := range []string{"/api/pull", "/api/delete", "/api/v1/models/load"} {
		if resp := sendRequest(t, http.MethodPost, srv.URL+p); resp.StatusCode != http.StatusOK {
			t.Errorf("POST %s: status = %d, want the upstream's 200", p, resp.StatusCode)
		}
	}
	resp := sendRequest(t, http.MethodGet, srv.URL+"/v1/models", "Connection", "Upgrade", "Upgrade", "h2c")
	if got := resp.Header.Get("X-Upgrade-Seen"); got != "h2c" {
		t.Errorf("h2c offer: upstream saw Upgrade %q, want it forwarded unchanged", got)
	}
	if n := hits.Load(); n != 4 {
		t.Errorf("upstream got %d requests, want 4", n)
	}
	if strings.Contains(logs.String(), "not forwarded") {
		t.Errorf("requests refused with forward_management: true:\n%s", logs)
	}
	checkWebSocketTunnel(t, srv.URL+"/llm")
}

// A 101 response body is the upgraded connection. Wrapping it in a
// meteredBody makes it non-writable and breaks the tunnel, so it must stay
// unwrapped on metered paths too.
func TestUpgradeOnMeteredPathNotWrapped(t *testing.T) {
	target, _ := startCountingUpstream(t)
	srv, logs := startProxy(t, target)
	cfg.ForwardManagement = true
	cfg.Logging.Debug = true

	checkWebSocketTunnel(t, srv.URL+"/v1/realtime")
	if want := "protocol upgrade, connection not inspected"; !strings.Contains(logs.String(), want) {
		t.Errorf("logs missing %q:\n%s", want, logs)
	}
	metrics.mu.RLock()
	defer metrics.mu.RUnlock()
	if metrics.QueryCount != 0 {
		t.Errorf("QueryCount = %d, want 0 for an upgraded connection", metrics.QueryCount)
	}
}

// checkWebSocketTunnel opens a WebSocket through the proxy and checks that
// bytes sent over it come back from the upstream.
func checkWebSocketTunnel(t *testing.T, url string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(webSocketHandshake); i += 2 {
		req.Header.Set(webSocketHandshake[i], webSocketHandshake[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	conn, ok := resp.Body.(io.ReadWriteCloser)
	if resp.StatusCode != http.StatusSwitchingProtocols || !ok {
		t.Fatalf("status = %d, writable body %t; want 101 and an upgraded connection", resp.StatusCode, ok)
	}
	conn.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Errorf("echo over the tunnel = %q, %v; want \"ping\"", buf, err)
	}
}

func TestNonInferenceResponseNotCounted(t *testing.T) {
	target := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"object":"list","data":[{"id":"qwen2.5-7b-instruct","object":"model"}]}`)
	}))
	srv, logs := startProxy(t, target)

	resp, err := http.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, resp.Body)
	resp.Body.Close()

	waitFor(t, "request log line", func() bool {
		return strings.Contains(logs.String(), "[Proxy] GET /v1/models 200")
	})
	metrics.mu.RLock()
	defer metrics.mu.RUnlock()
	if metrics.QueryCount != 0 {
		t.Errorf("QueryCount = %d, want 0", metrics.QueryCount)
	}
}

func readCSV(t *testing.T, path string) [][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestCSVLogAppendsWithSingleHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.csv")
	info := &requestInfo{method: "POST", path: "/api/v1/chat"}
	now := time.Date(2026, 10, 17, 14, 2, 31, 500e6, time.UTC)
	full := MetricsData{
		ModelName: "qwen, \"8b\"", PromptTokens: 42, CompletionTokens: 311, TotalTokens: 353,
		TokensPerSecond: 48.123, TimeToFirstToken: 210 * time.Millisecond, Duration: 6680 * time.Millisecond,
	}
	sparse := MetricsData{ModelName: "m", Duration: time.Second}

	// Two runs of the proxy appending to the same file.
	for _, rows := range [][]MetricsData{{full}, {sparse}} {
		rec, err := openCSVLog(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range rows {
			if err := rec.record(now, info, 200, d, d.TotalTokens > 0); err != nil {
				t.Fatal(err)
			}
		}
		if err := rec.Close(); err != nil {
			t.Fatal(err)
		}
		if err := rec.record(now, info, 200, sparse, true); err != nil {
			t.Errorf("record after Close = %v, want it silently dropped", err)
		}
	}

	want := [][]string{
		csvHeader,
		{"2026-10-17T14:02:31.500Z", "POST", "/api/v1/chat", "200", "qwen, \"8b\"", "42", "311", "353", "48.12", "false", "0.210", "6.680", "true"},
		{"2026-10-17T14:02:31.500Z", "POST", "/api/v1/chat", "200", "m", "", "", "", "", "false", "", "1.000", "false"},
	}
	if got := readCSV(t, path); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("CSV rows:\n got %q\nwant %q", got, want)
	}
}

func TestCSVNeutralizesUntrustedText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.csv")
	rec, err := openCSVLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	values := []struct{ raw, want string }{
		{"=1+1", "'=1+1"}, {"+1+1", "'+1+1"}, {"-1+1", "'-1+1"}, {"@SUM(A1)", "'@SUM(A1)"},
		{" \t=1+1", "' \t=1+1"}, {"\r+1", "'\r+1"}, {"\n+1", "'\n+1"}, {"\ttext", "'\ttext"},
		{"\u00a0\ufeff=1", "'\u00a0\ufeff=1"}, {"＝1+1", "'＝1+1"}, {"＋1", "'＋1"}, {"－1", "'－1"}, {"＠SUM(A1)", "'＠SUM(A1)"},
		{"\xff=1", "'\xff=1"}, {" \x00@SUM(A1)", "' \x00@SUM(A1)"},
		{"", ""}, {"plain", "plain"}, {"qwen, \"8b\"", "qwen, \"8b\""}, {"/v1/=model", "/v1/=model"}, {"'literal", "'literal"},
		{"  qwen-ä-日本-👩‍💻", "  qwen-ä-日本-👩‍💻"},
	}
	for _, tt := range values {
		info := &requestInfo{method: tt.raw, path: tt.raw}
		if err := rec.record(time.Now(), info, 200, MetricsData{ModelName: tt.raw}, true); err != nil {
			t.Fatal(err)
		}
	}
	rows := readCSV(t, path)
	for i, tt := range values {
		for _, column := range []int{1, 2, 4} {
			if got := rows[i+1][column]; got != tt.want {
				t.Errorf("input %q, column %d: got %q, want %q", tt.raw, column, got, tt.want)
			}
		}
	}
}

func TestProxyCSVNeutralizesModelAliases(t *testing.T) {
	for _, field := range []string{"model", "model_instance_id"} {
		t.Run(field, func(t *testing.T) { checkProxyCSVFormula(t, field) })
	}
}

func checkProxyCSVFormula(t *testing.T, field string) {
	t.Helper()
	model := " \t=1+1"
	body, err := json.Marshal(map[string]string{field: model})
	if err != nil {
		t.Fatal(err)
	}
	target := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	srv, _ := startProxy(t, target)
	path := filepath.Join(t.TempDir(), "stats.csv")
	rec, err := openCSVLog(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	csvLog = rec
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if readAll(t, resp.Body) != string(body) {
		t.Fatal("CSV escaping changed the forwarded response")
	}
	// QueryCount can advance before the CSV row is flushed.
	waitFor(t, "CSV formula row", func() bool { return len(readCSV(t, path)) == 2 })
	if got := readCSV(t, path)[1][4]; got != "'"+model {
		t.Errorf("model cell = %q, want %q", got, "'"+model)
	}
	metrics.mu.RLock()
	defer metrics.mu.RUnlock()
	if metrics.ModelName != model {
		t.Errorf("CSV escaping changed aggregate model to %q", metrics.ModelName)
	}
}

func TestProxyWritesQueriesToCSV(t *testing.T) {
	target := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"object":"list","data":[]}`)
			return
		}
		io.WriteString(w, chatCompletionJSON)
	}))
	srv, _ := startProxy(t, target)
	path := filepath.Join(t.TempDir(), "stats.csv")
	rec, err := openCSVLog(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	csvLog = rec

	for _, p := range []string{"/v1/models", "/v1/chat/completions"} {
		resp, err := http.Post(srv.URL+p, "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		readAll(t, resp.Body)
		resp.Body.Close()
	}
	waitForQueries(t, 1)

	rows := readCSV(t, path)
	if len(rows) != 2 {
		t.Fatalf("want header + 1 query row (models request not written), got %q", rows)
	}
	// Non-streamed OpenAI response without a reported rate: TPS is estimated.
	row := rows[1]
	if row[2] != "/v1/chat/completions" || row[4] != "qwen2.5-7b-instruct" || row[7] != "30" || row[9] != "true" || row[12] != "true" {
		t.Errorf("unexpected row %q", row)
	}
}

func TestOpenCSVLogErrors(t *testing.T) {
	if _, err := openCSVLog(t.TempDir()); err == nil {
		t.Error("openCSVLog(directory): want an error")
	}
	if runtime.GOOS != "linux" {
		t.Skip("header write error needs /dev/full")
	}
	// /dev/full opens and is empty, but every write fails, so the header can't be written.
	if _, err := openCSVLog("/dev/full"); err == nil {
		t.Error("openCSVLog(/dev/full): want the header write error")
	}
}

// A failing CSV write (full disk, removed drive) is logged; proxying and the
// in-memory metrics carry on.
func TestCSVWriteErrorLogged(t *testing.T) {
	target := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, chatCompletionJSON)
	}))
	srv, logs := startProxy(t, target)
	rec, err := openCSVLog(filepath.Join(t.TempDir(), "stats.csv"))
	if err != nil {
		t.Fatal(err)
	}
	rec.f.Close() // later writes fail
	t.Cleanup(func() {
		rec.Close()
		if err := rec.Close(); err != nil {
			t.Errorf("second Close() = %v, want nil", err)
		}
	})
	csvLog = rec

	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if body := readAll(t, resp.Body); resp.StatusCode != http.StatusOK || body != chatCompletionJSON {
		t.Errorf("client got %d %q, want the upstream's response", resp.StatusCode, body)
	}
	waitForQueries(t, 1)
	waitFor(t, "CSV error line", func() bool { return strings.Contains(logs.String(), "[ERROR] writing CSV: ") })
}

func TestCaptureDiag(t *testing.T) {
	tests := []struct {
		contentType, body string
		want              captureDiag
	}{
		{"application/json", chatCompletionJSON, captureDiag{name: "JSON", documents: 1}},
		{"application/json", "not json", captureDiag{name: "JSON", invalid: 1}},
		{"application/json", "", captureDiag{name: "JSON"}},
		{"text/event-stream", "data: {\"model\":\"m\"}\n\ndata: oops\n\n: comment\n\ndata: [DONE]\n\n", captureDiag{name: "SSE", documents: 1, invalid: 1}},
		{"application/x-ndjson", "{\"model\":\"m\"}\n\n[1,2]\n{\"done\":true}", captureDiag{name: "NDJSON", documents: 2, invalid: 1}},
	}
	for _, tt := range tests {
		c := newCapture(tt.contentType)
		io.WriteString(c, tt.body)
		c.finish()
		if got := c.diag(); got != tt.want {
			t.Errorf("%s %q: diag = %+v, want %+v", tt.contentType, tt.body, got, tt.want)
		}
	}

	var s lineSplitter
	s.write(bytes.Repeat([]byte("x"), maxCaptureBytes+1), func([]byte) {})
	s.write([]byte("\nshort\n"), func([]byte) {})
	if s.oversized != 1 {
		t.Errorf("lineSplitter.oversized = %d, want 1", s.oversized)
	}
}

// debugTestUpstream serves the responses used by the debug logging tests.
func debugTestUpstream(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/chat/completions": // non-streamed, usage but no rate: TPS estimated
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, chatCompletionJSON)
	case "/v1/stream": // streamed without include_usage
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"model\":\"m\",\"choices\":[]}\n\ndata: oops\n\ndata: [DONE]\n\n")
	case "/v1/models": // JSON without inference data
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"object":"list","data":[]}`)
	default:
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "hello")
	}
}

// postAndDrain sends an empty JSON request through the proxy and reads the
// whole response.
func postAndDrain(t *testing.T, url string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	readAll(t, resp.Body)
}

func TestDebugLoggingOffByDefault(t *testing.T) {
	srv, logs := startProxy(t, startUpstream(t, http.HandlerFunc(debugTestUpstream)))
	postAndDrain(t, srv.URL+"/v1/chat/completions")
	waitForQueries(t, 1)
	if strings.Contains(logs.String(), "[DEBUG]") {
		t.Errorf("debug lines printed with logging.debug off:\n%s", logs)
	}
}

func TestDebugLogging(t *testing.T) {
	target := startUpstream(t, http.HandlerFunc(debugTestUpstream))
	tests := []struct {
		path string
		want []string
	}{
		{"/v1/chat/completions", []string{
			"POST /v1/chat/completions from 127.0.0.1:",
			"upstream http://" + target.Host + "/v1/chat/completions -> 200 after",
			`Content-Type "application/json"`,
			fmt.Sprintf("body ended at EOF after %d bytes", len(chatCompletionJSON)),
			"parsed as JSON: 1 JSON document(s), 0 invalid, 0 skipped",
			"estimated as completion tokens / request duration",
		}},
		{"/v1/stream", []string{
			"parsed as SSE: 1 JSON document(s), 1 invalid, 0 skipped",
			`must send "stream_options": {"include_usage": true}`,
		}},
		{"/v1/models", []string{
			"parsed as JSON: 1 JSON document(s), 0 invalid, 0 skipped",
			"no model, token or rate fields found; not counted as a query",
		}},
		{"/v1/text", []string{
			`Content-Type "text/plain"`,
			"Content-Type not inspected",
			"body ended at EOF after 5 bytes",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) { checkDebugOutput(t, target, tt.path, tt.want) })
	}
}

// checkDebugOutput sends one request with logging.debug on and checks that
// the debug lines contain want and share one request ID.
func checkDebugOutput(t *testing.T, target *url.URL, path string, want []string) {
	t.Helper()
	srv, logs := startProxy(t, target)
	cfg.Logging.Debug = true
	postAndDrain(t, srv.URL+path)
	waitFor(t, "the request to finish", func() bool { return strings.Contains(logs.String(), "body ended") })

	got := logs.String()
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("debug output missing %q:\n%s", w, got)
		}
	}
	if ids := debugRequestIDs(got); len(ids) != 1 {
		t.Errorf("want one request ID across debug lines, got %v", ids)
	}
}

// debugRequestIDs returns the request IDs ("req N:") found in debug lines.
func debugRequestIDs(logs string) map[string]bool {
	ids := map[string]bool{}
	for line := range strings.SplitSeq(logs, "\n") {
		if rest, ok := strings.CutPrefix(line, "[DEBUG] req "); ok {
			ids[strings.SplitN(rest, ":", 2)[0]] = true
		}
	}
	return ids
}

func TestConcurrentRequests(t *testing.T) {
	target := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, chatCompletionJSON)
	}))
	srv, _ := startProxy(t, target)

	const n = 25
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
			if err != nil {
				t.Error(err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		})
	}
	wg.Wait()

	waitForQueries(t, n)
	metrics.mu.RLock()
	defer metrics.mu.RUnlock()
	if metrics.QueryCount != n || metrics.TotalTokens != n*30 {
		t.Errorf("QueryCount = %d, TotalTokens = %d; want %d, %d", metrics.QueryCount, metrics.TotalTokens, n, n*30)
	}
	if len(metrics.History) != cfg.Metrics.HistoryPoints {
		t.Errorf("len(History) = %d, want %d", len(metrics.History), cfg.Metrics.HistoryPoints)
	}
}

func TestUpdateGlobalMetrics(t *testing.T) {
	cfg = Config{Metrics: MetricsConfig{HistoryPoints: 2}}
	resetMetrics()

	var snap metricsSnapshot
	for i := 1; i <= 3; i++ {
		snap = updateGlobalMetrics(MetricsData{
			ModelName:       "m",
			TotalTokens:     i * 10,
			TokensPerSecond: float64(i * 10),
			Duration:        time.Duration(i) * time.Second,
		})
	}

	want := metricsSnapshot{QueryCount: 3, TotalTokens: 60, AvgTimePerQuery: 2 * time.Second, AvgTokensPerSec: 25}
	if snap != want {
		t.Errorf("snapshot = %+v, want %+v", snap, want)
	}
	if len(metrics.History) != 2 || metrics.History[1].TokensPerSec != 30 {
		t.Errorf("history should hold the newest 2 points, newest last: %+v", metrics.History)
	}
	if metrics.TokensPerSecond != 30 {
		t.Errorf("TokensPerSecond = %v, want most recent 30", metrics.TokensPerSecond)
	}
}

func TestCompleteDerived(t *testing.T) {
	tests := []struct {
		name       string
		in         MetricsData
		streamTime time.Duration
		wantTotal  int
		wantTPS    float64
		wantEst    bool
	}{
		{"reported rate kept", MetricsData{CompletionTokens: 10, TokensPerSecond: 7, Duration: time.Second}, 0, 10, 7, false},
		{"ttft excluded", MetricsData{PromptTokens: 5, CompletionTokens: 10, TimeToFirstToken: time.Second, Duration: 3 * time.Second}, 0, 15, 5, true},
		{"stream time used", MetricsData{CompletionTokens: 10, Duration: 3 * time.Second}, 2 * time.Second, 10, 5, true},
		{"whole duration fallback", MetricsData{CompletionTokens: 10, Duration: 2 * time.Second}, 0, 10, 5, true},
		{"no completion tokens", MetricsData{PromptTokens: 4, Duration: time.Second}, 0, 4, 0, false},
	}
	for _, tt := range tests {
		d := tt.in
		basis := d.completeDerived(tt.streamTime)
		if d.TotalTokens != tt.wantTotal || d.TokensPerSecond != tt.wantTPS || d.TPSEstimated != tt.wantEst {
			t.Errorf("%s: total %d, tps %v, est %v; want %d, %v, %v",
				tt.name, d.TotalTokens, d.TokensPerSecond, d.TPSEstimated, tt.wantTotal, tt.wantTPS, tt.wantEst)
		}
		if (basis != "") != tt.wantEst {
			t.Errorf("%s: basis %q, want one exactly when the rate is estimated", tt.name, basis)
		}
	}
}

func TestFormatMetricsLineMarksEstimatedTPS(t *testing.T) {
	info := &requestInfo{method: "POST", path: "/v1/chat/completions"}
	snap := metricsSnapshot{QueryCount: 1}
	for _, tt := range []struct {
		est  bool
		want string
	}{
		{false, "| TPS: 40.00 |"},
		{true, "| TPS: 40.00 (est.) |"},
	} {
		line := formatMetricsLine(time.Now(), info, 200, MetricsData{TokensPerSecond: 40, TPSEstimated: tt.est}, snap, true)
		if !strings.Contains(line, tt.want) {
			t.Errorf("estimated=%v: line %q does not contain %q", tt.est, line, tt.want)
		}
	}
}

func TestFormatMetricsLineTokens(t *testing.T) {
	info := &requestInfo{method: "POST", path: "/v1/embeddings"}
	snap := metricsSnapshot{QueryCount: 1}
	for _, tt := range []struct {
		data MetricsData
		want string
	}{
		{MetricsData{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7}, "| Tokens: 5 in / 2 out |"},
		{MetricsData{TotalTokens: 7}, "| Tokens: 7 |"}, // only a total reported
	} {
		if line := formatMetricsLine(time.Now(), info, 200, tt.data, snap, true); !strings.Contains(line, tt.want) {
			t.Errorf("%+v: line %q does not contain %q", tt.data, line, tt.want)
		}
	}
}

func TestDetectBackend(t *testing.T) {
	// Responses modeled on the real servers: LM Studio answers unknown
	// endpoints with 200 and an error body, Ollama with 404.
	lmUnknown := `{"error":"Unexpected endpoint or method."}`
	servers := map[string]map[string]string{
		"Ollama": {
			"/api/version": `{"version":"0.40.2"}`,
			"/v1/models":   `{"object":"list","data":[]}`,
		},
		"LM Studio": {
			"/api/version":   lmUnknown,
			"/api/v1/models": `{"models":[]}`,
			"/v1/models":     `{"data":[],"object":"list"}`,
		},
		"OpenAI-compatible server": {
			"/v1/models": `{"object":"list","data":[]}`,
		},
		"": {
			"/api/version": "Not JSON",
		},
	}
	for want, routes := range servers {
		target := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, ok := routes[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, body)
		}))
		if got := detectBackend(target); got != want {
			t.Errorf("detectBackend() = %q, want %q", got, want)
		}
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	u, _ := url.Parse(closed.URL)
	closed.Close()
	if got := detectBackend(u); got != "" {
		t.Errorf("unreachable upstream: detectBackend() = %q, want \"\"", got)
	}
}

func TestTruncateString(t *testing.T) {
	if got := truncateString("short", 30); got != "short" {
		t.Errorf("got %q", got)
	}
	if got := truncateString("ääääää", 5); got != "ää..." {
		t.Errorf("got %q, want rune-safe truncation", got)
	}
}

func TestEscapeControls(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"/v1/chat/completions?stream=true", "/v1/chat/completions?stream=true"},
		{"\x1b[2J\x1b]0;title\a", `\x1b[2J\x1b]0;title\a`},
		{"/a\r\n[12:00:00] #1 fake", `/a\r\n[12:00:00] #1 fake`},
		{"\x00\t\x7f", `\x00\t\x7f`},
		{"\u009b31m\u0085", `\u009b31m\u0085`},                                                         // C1: CSI, NEL
		{"a\u2028b\u2029c", `a\u2028b\u2029c`},                                                         // line and paragraph separators
		{"\u202eabc\u202c\u2066\u2069\u200f\u061c", `\u202eabc\u202c\u2066\u2069\u200f\u061c`},         // bidi controls
		{"\xff\xc0x\xe2\x80", `\xff\xc0x\xe2\x80`},                                                     // invalid UTF-8
		{`C:\dir\x1b "q"`, `C:\dir\x1b "q"`},                                                           // backslashes and quotes kept
		{"qwen3:4b-\u00e4-\u65e5\u672c-\ufffd", "qwen3:4b-\u00e4-\u65e5\u672c-\ufffd"},                 // printable non-ASCII, real U+FFFD
		{"\U0001f469\u200d\U0001f4bb soft\u00adhyphen", "\U0001f469\u200d\U0001f4bb soft\u00adhyphen"}, // other format characters
	}
	for _, tt := range tests {
		if got := escapeControls(tt.in); got != tt.want {
			t.Errorf("escapeControls(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestEscapeControlsLossless escapes every byte, every rune up to U+00FF and
// the escaped separators and bidi controls on their own. The result must have
// nothing left to escape and unquote back to the input as a Go string.
func TestEscapeControlsLossless(t *testing.T) {
	var inputs []string
	for c := range 256 {
		inputs = append(inputs, string([]byte{byte(c)}), string(rune(c)))
	}
	for _, r := range []rune{0x061c, 0x200e, 0x200f, 0x2028, 0x2029, 0x202a, 0x202e, 0x2066, 0x2069} {
		inputs = append(inputs, string(r))
	}
	for _, in := range inputs {
		checkEscapedLossless(t, in)
	}
}

func checkEscapedLossless(t *testing.T, in string) {
	t.Helper()
	got := escapeControls(in)
	if unsafeInLog(got) {
		t.Errorf("escapeControls(%q) = %q, still contains characters to escape", in, got)
	}
	if !unsafeInLog(in) && got != in {
		t.Errorf("escapeControls(%q) = %q, want it unchanged", in, got)
	}
	if in == `"` || in == `\` {
		return // kept as is, so not a valid Go string body
	}
	if back, err := strconv.Unquote(`"` + got + `"`); err != nil || back != in {
		t.Errorf("escapeControls(%q) = %q, unquotes to %q (%v)", in, got, back, err)
	}
}

// unsafeInLog reports whether s contains what escapeControls escapes:
// invalid UTF-8, control characters, line or paragraph separators, or bidi
// controls.
func unsafeInLog(s string) bool {
	return !utf8.ValidString(s) || strings.IndexFunc(s, func(r rune) bool {
		return unicode.IsControl(r) || unicode.In(r, unicode.Bidi_Control, unicode.Zl, unicode.Zp)
	}) >= 0
}

// Status lines print the path of every request, so text with nothing to
// escape must not be copied.
func TestEscapeControlsCleanTextNotCopied(t *testing.T) {
	clean := "/v1/chat/completions?model=qwen3:4b&q=\u00e4\u65e5\u672c"
	if allocs := testing.AllocsPerRun(100, func() { _ = escapeControls(clean) }); allocs != 0 {
		t.Errorf("escapeControls allocated %v times for text with nothing to escape", allocs)
	}
}

// injectedPath decodes to a clear-screen escape sequence, CR LF, a fake
// status line and a bidi override; injectedLog is how it must be printed.
const (
	injectedPath = "/x%1b[2J%0d%0a[12:00:00]%20fake%e2%80%ae"
	injectedLog  = `/x\x1b[2J\r\n[12:00:00] fake\u202e`
)

func TestLogEscapesControlCharacters(t *testing.T) {
	t.Run("not metered, debug", testEscapedNotMetered)
	t.Run("not forwarded", testEscapedNotForwarded)
	t.Run("upstream unreachable", testEscapedUpstreamError)
	t.Run("no inference", testEscapedNoInference)
	t.Run("query and model name", testEscapedQuery)
}

func testEscapedNotMetered(t *testing.T) {
	srv, logs := startProxy(t, startUpstream(t, http.NotFoundHandler()))
	cfg.Logging.Debug = true
	postAndDrain(t, srv.URL+injectedPath+"?q=\u009b") // raw C1 CSI in the query
	checkEscapedLogs(t, logs, 3, "[Proxy] POST "+injectedLog+" 404 (", `?q=\u009b from `, " -> 404 after ")
}

func testEscapedNotForwarded(t *testing.T) {
	srv, logs := startProxy(t, startUpstream(t, http.NotFoundHandler()))
	forward := false
	cfg.ForwardUnmatched = &forward
	postAndDrain(t, srv.URL+injectedPath)
	checkEscapedLogs(t, logs, 1, "[Proxy] Non-API request: POST "+injectedLog+"\n")
}

func testEscapedUpstreamError(t *testing.T) {
	up := httptest.NewServer(http.NotFoundHandler())
	target, _ := url.Parse(up.URL)
	up.Close()
	srv, logs := startProxy(t, target)
	postAndDrain(t, srv.URL+"/v1"+injectedPath)
	checkEscapedLogs(t, logs, 1, "[ERROR] POST /v1"+injectedLog+": ")
}

func testEscapedNoInference(t *testing.T) {
	srv, logs := startProxy(t, startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"object":"list","data":[]}`)
	})))
	postAndDrain(t, srv.URL+"/v1"+injectedPath)
	checkEscapedLogs(t, logs, 1, "[Proxy] POST /v1"+injectedLog+" 200 (")
}

func testEscapedQuery(t *testing.T) {
	srv, logs := startProxy(t, startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"m\u001b[2J\r\n[12:00:00] #9\u202e",`+
			`"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
	})))
	postAndDrain(t, srv.URL+"/v1"+injectedPath)
	checkEscapedLogs(t, logs, 1, " POST /v1"+injectedLog+` 200 | Model: m\x1b[2J\r\n[12:00:00] #9\u202e | Tokens: 1 in / 2 out`)
}

// checkEscapedLogs waits for wantLines log lines, then checks that they
// contain each of want and nothing that escapeControls escapes.
func checkEscapedLogs(t *testing.T, logs *syncBuffer, wantLines int, want ...string) {
	t.Helper()
	waitFor(t, fmt.Sprintf("%d log lines", wantLines), func() bool {
		return strings.Count(logs.String(), "\n") >= wantLines
	})
	got := logs.String()
	if n := strings.Count(got, "\n"); n != wantLines {
		t.Errorf("got %d log lines, want %d: %q", n, wantLines, got)
	}
	if unsafeInLog(strings.ReplaceAll(got, "\n", "")) {
		t.Errorf("logs contain unescaped control characters: %q", got)
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("logs missing %q: %q", w, got)
		}
	}
}

// TestAllowedHost checks which Host header values are accepted: names a DNS
// rebinding attack can't use, plus allowed_hosts.
func TestAllowedHost(t *testing.T) {
	t.Cleanup(func() { cfg = Config{} })
	cfg = Config{}
	hostname, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	checkAllowedHosts(t, true, "", "127.0.0.1:11436", "192.0.2.10", "192.0.2.10:11436", "[::1]:11436", "[::1]", "::1",
		"[2001:db8::1]:80", "localhost", "LocalHost:11436", "localhost.:11436", "app.localhost", "printer.local:11436",
		"llm.internal", "homelab.home.arpa", hostname, strings.ToUpper(hostname)+":11436")
	checkAllowedHosts(t, false, "evil.example", "evil.example:11436", "evil.example.:11436", "[evil.example]",
		"localhost.evil.example", "127.0.0.1.nip.io", "evil-localhost", "llm-proxy-test.lan")

	cfg.AllowedHosts = []string{"llm.example.com", "*.LAN"}
	checkAllowedHosts(t, true, "LLM.example.com:11436", "llm.example.com.", "llm-proxy-test.lan", "a.b.lan:11436")
	checkAllowedHosts(t, false, "evil.example", "lan", "x.llm.example.com", "llm.example.com.evil.example")

	cfg.AllowedHosts = []string{"*"}
	checkAllowedHosts(t, true, "evil.example:11436")
}

func checkAllowedHosts(t *testing.T, want bool, hosts ...string) {
	t.Helper()
	for _, h := range hosts {
		if got := allowedHost(hostName(h)); got != want {
			t.Errorf("allowedHost(hostName(%q)) = %t, want %t (allowed_hosts %q)", h, got, want, cfg.AllowedHosts)
		}
	}
}

// getWithHost sends a GET request with the given Host header and returns the
// response status.
func getWithHost(t *testing.T, url, host string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	readAll(t, resp.Body)
	return resp.StatusCode
}

// TestProxyRejectsRebindingHost sends requests as a DNS rebinding page would
// (Host naming the attacker's domain): they must get 403 without reaching the
// upstream, while local names and allowed_hosts are forwarded.
func TestProxyRejectsRebindingHost(t *testing.T) {
	var hits atomic.Int32
	target := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"models":[]}`)
	}))
	srv, logs := startProxy(t, target)
	cfg.AllowedHosts = []string{"llm.example.com"}

	for _, path := range []string{"/api/tags", "/v1/models"} { // unmetered and metered
		if status := getWithHost(t, srv.URL+path, "evil.example:11436"); status != http.StatusForbidden {
			t.Errorf("%s with Host evil.example: status = %d, want 403", path, status)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("upstream got %d request(s) with a rejected Host, want 0", n)
	}
	want := `[Proxy] Rejected GET /api/tags: Host "evil.example:11436" not allowed (add "evil.example" to allowed_hosts`
	if !strings.Contains(logs.String(), want) {
		t.Errorf("logs missing %q:\n%s", want, logs)
	}

	hostname, _ := os.Hostname()
	accepted := []string{"localhost:11436", "192.0.2.10:11436", "[::1]:11436", hostname, "llm.example.com:11436"}
	for _, host := range accepted {
		if status := getWithHost(t, srv.URL+"/api/tags", host); status != http.StatusOK {
			t.Errorf("Host %q: status = %d, want 200", host, status)
		}
	}
	if n := hits.Load(); n != int32(len(accepted)) {
		t.Errorf("upstream got %d request(s), want %d", n, len(accepted))
	}
}

func TestParseConfigAllowedHosts(t *testing.T) {
	c, err := parseConfig([]byte("allowed_hosts: [\"llm.example.com\", \"*.lan\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(c.AllowedHosts) != "[llm.example.com *.lan]" {
		t.Errorf("AllowedHosts = %q", c.AllowedHosts)
	}
	if c, err := parseConfig(nil); err != nil || len(c.AllowedHosts) != 0 {
		t.Errorf("default AllowedHosts = %q, %v; want none", c.AllowedHosts, err)
	}
}
