package main

import (
	"bufio"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// Fixtures in testdata/ are real responses captured from LM Studio and Ollama.

func TestIsAPIEndpoint(t *testing.T) {
	cfg.APIEndpoints = []string{"/api/v1/*", "/v1"}

	tests := []struct {
		path string
		want bool
	}{
		{"/api/v1/chat", true},
		{"/api/v1/models", true},
		{"/v1/chat/completions", true},
		{"/api/v0/chat", false},
		{"/api/v1", false},
		{"/", false},
	}
	for _, tt := range tests {
		if got := isAPIEndpoint(tt.path); got != tt.want {
			t.Errorf("isAPIEndpoint(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestResponseParserFixtures(t *testing.T) {
	tests := []struct {
		file     string
		streamed bool
		model    string
		tokens   int
		tps      float64
	}{
		{"lm_v1_chat.json", false, "qwen/qwen3-vl-4b", 15, 65.5780706931602},
		{"lm_v1_chat_stream.sse", true, "qwen/qwen3-vl-4b", 15, 76.20208793720948},
		{"lm_oai.json", false, "qwen/qwen3-vl-4b", 15, 0},
		{"lm_oai_stream.sse", true, "qwen/qwen3-vl-4b", 15, 0},
		{"ol_chat.json", false, "qwen3:4b-instruct", 15, 2 / 0.036687},
		{"ol_chat_stream.ndjson", true, "qwen3:4b-instruct", 15, 2 / 0.025315},
		{"ol_oai_stream.sse", true, "qwen3:4b-instruct", 15, 86.95652173913044},
	}
	for _, tt := range tests {
		body, err := os.ReadFile("testdata/" + tt.file)
		if err != nil {
			t.Fatal(err)
		}
		// Feed the body whole and byte by byte to exercise chunk boundaries
		for _, chunkSize := range []int{len(body), 1} {
			p := responseParser{streamed: tt.streamed}
			for b := body; len(b) > 0; {
				n := min(chunkSize, len(b))
				p.Write(b[:n])
				b = b[n:]
			}
			got := p.Result()
			if got.ModelName != tt.model || got.TotalTokens != tt.tokens || !approxEqual(got.TokensPerSecond, tt.tps) {
				t.Errorf("%s (chunk %d): got model=%q tokens=%d tps=%v, want model=%q tokens=%d tps=%v",
					tt.file, chunkSize, got.ModelName, got.TotalTokens, got.TokensPerSecond,
					tt.model, tt.tokens, tt.tps)
			}
		}
	}
}

func TestResponseParserEdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		streamed bool
		body     string
		want     MetricsData
	}{
		{"non-JSON body", false, "Not Found", MetricsData{}},
		{"truncated JSON", false, `{"model":"m","usage":{"total_to`, MetricsData{}},
		{"CRLF SSE without trailing newline", true,
			"event: chat.end\r\ndata: {\"result\":{\"model_instance_id\":\"m\",\"stats\":{\"input_tokens\":1,\"total_output_tokens\":2,\"tokens_per_second\":3.5}}}",
			MetricsData{ModelName: "m", TotalTokens: 3, TokensPerSecond: 3.5}},
		{"field type mismatch keeps other fields", false,
			`{"model":{"unexpected":"object"},"usage":{"total_tokens":7}}`,
			MetricsData{TotalTokens: 7}},
		{"oversized body is not parsed", false,
			`{"model":"m","pad":"` + strings.Repeat("x", maxParseBytes) + `"}`,
			MetricsData{}},
		{"oversized stream line is skipped", true,
			"data: {\"pad\":\"" + strings.Repeat("x", maxParseBytes) + "\"}\n" +
				"data: {\"model\":\"m\",\"usage\":{\"total_tokens\":4}}\n",
			MetricsData{ModelName: "m", TotalTokens: 4}},
	}
	for _, tt := range tests {
		p := responseParser{streamed: tt.streamed}
		p.Write([]byte(tt.body))
		if got := p.Result(); got != tt.want {
			t.Errorf("%s: got %+v, want %+v", tt.name, got, tt.want)
		}
	}
}

func TestProxyForwardsAndRecordsMetrics(t *testing.T) {
	fixture, err := os.ReadFile("testdata/lm_v1_chat.json")
	if err != nil {
		t.Fatal(err)
	}
	const reqBody = `{"model":"qwen/qwen3-vl-4b","input":"hi"}`

	proxyURL, done := startProxy(t, func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/chat" || r.URL.RawQuery != "a=1" ||
			string(got) != reqBody || r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("upstream got %s %s?%s body=%q auth=%q",
				r.Method, r.URL.Path, r.URL.RawQuery, got, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-Upstream", "lmstudio")
		w.Write(fixture)
	})

	req, _ := http.NewRequest(http.MethodPost, proxyURL+"/api/v1/chat?a=1", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	waitForRequest(t, done)

	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Upstream") != "lmstudio" {
		t.Errorf("got status %d, X-Upstream %q", resp.StatusCode, resp.Header.Get("X-Upstream"))
	}
	if string(body) != string(fixture) {
		t.Errorf("client body differs from upstream body:\n%s", body)
	}
	assertMetrics(t, 1, "qwen/qwen3-vl-4b", 15, 65.5780706931602)
}

func TestProxyStreamsIncrementally(t *testing.T) {
	release := make(chan struct{})
	proxyURL, done := startProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: chat.start\ndata: {\"type\":\"chat.start\",\"model_instance_id\":\"m\"}\n\n")
		w.(http.Flusher).Flush()
		<-release
		io.WriteString(w, "event: chat.end\ndata: {\"type\":\"chat.end\",\"result\":{\"model_instance_id\":\"m\","+
			"\"stats\":{\"input_tokens\":10,\"total_output_tokens\":20,\"tokens_per_second\":42.5}}}\n\n")
	})

	resp, err := http.Post(proxyURL+"/api/v1/chat", "application/json", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// The first event must reach the client while upstream is still generating
	firstLine := make(chan string, 1)
	reader := bufio.NewReader(resp.Body)
	go func() {
		line, _ := reader.ReadString('\n')
		firstLine <- line
	}()
	select {
	case line := <-firstLine:
		if line != "event: chat.start\n" {
			t.Errorf("first line = %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first SSE event was not streamed before the response completed")
	}

	close(release)
	rest, _ := io.ReadAll(reader)
	if !strings.Contains(string(rest), "event: chat.end") {
		t.Errorf("chat.end event missing from client stream:\n%s", rest)
	}
	waitForRequest(t, done)
	assertMetrics(t, 1, "m", 30, 42.5)
}

func TestProxyUpstreamUnavailable(t *testing.T) {
	upstream := httptest.NewServer(http.NotFoundHandler())
	target, _ := url.Parse(upstream.URL)
	upstream.Close() // nothing listens on target any more

	cfg.APIEndpoints = []string{"/api/v1/*"}
	resetMetrics()
	lmProxy = newProxy(target)
	proxy := httptest.NewServer(http.HandlerFunc(handleRequest))
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/api/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadGateway)
	}
	assertMetrics(t, 0, "", 0, 0)
}

func TestProxyRejectsNonAPIPaths(t *testing.T) {
	proxyURL, _ := startProxy(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream should not be called, got %s", r.URL.Path)
	})

	resp, err := http.Get(proxyURL + "/admin")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

// startProxy runs a fake upstream and a proxy in front of it. The returned
// channel receives once per proxied request, after its metrics are recorded.
func startProxy(t *testing.T, upstream http.HandlerFunc) (string, <-chan struct{}) {
	t.Helper()
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	target, _ := url.Parse(up.URL)

	cfg.APIEndpoints = []string{"/api/v1/*"}
	cfg.Metrics.HistoryPoints = 10
	resetMetrics()
	lmProxy = newProxy(target)

	done := make(chan struct{}, 10)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handleRequest(w, r)
		done <- struct{}{}
	}))
	t.Cleanup(proxy.Close)
	return proxy.URL, done
}

func waitForRequest(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("proxied request did not complete")
	}
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

func assertMetrics(t *testing.T, queries int, model string, tokens int, tps float64) {
	t.Helper()
	metrics.mu.RLock()
	defer metrics.mu.RUnlock()
	if metrics.QueryCount != queries || metrics.ModelName != model ||
		metrics.TotalTokens != tokens || !approxEqual(metrics.TokensPerSecond, tps) ||
		len(metrics.History) != queries {
		t.Errorf("metrics = queries:%d model:%q tokens:%d tps:%v history:%d, want queries:%d model:%q tokens:%d tps:%v",
			metrics.QueryCount, metrics.ModelName, metrics.TotalTokens, metrics.TokensPerSecond, len(metrics.History),
			queries, model, tokens, tps)
	}
	if queries > 0 && metrics.AvgTimePerQuery <= 0 {
		t.Errorf("AvgTimePerQuery = %v, want > 0", metrics.AvgTimePerQuery)
	}
}

func approxEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-6
}
