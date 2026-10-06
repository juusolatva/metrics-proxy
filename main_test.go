package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
	c, err := parseConfig([]byte("api_endpoints: [\"/v1/*\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.ProxyPort != 8080 || c.LmStudio.Host != "localhost" || c.LmStudio.Port != 1234 || c.Metrics.HistoryPoints != 10 {
		t.Errorf("defaults not applied: %+v", c)
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
	// request is made inside the timed goroutine.
	type firstEvent struct {
		resp  *http.Response
		br    *bufio.Reader
		event string
		err   error
	}
	gotFirst := make(chan firstEvent, 1)
	go func() {
		resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"stream":true}`))
		if err != nil {
			gotFirst <- firstEvent{err: err}
			return
		}
		br := bufio.NewReader(resp.Body)
		var sb strings.Builder
		for !strings.HasSuffix(sb.String(), "\n\n") {
			line, err := br.ReadString('\n')
			sb.WriteString(line)
			if err != nil {
				break
			}
		}
		gotFirst <- firstEvent{resp: resp, br: br, event: sb.String()}
	}()

	var fe firstEvent
	select {
	case fe = <-gotFirst:
	case <-time.After(2 * time.Second):
		t.Fatal("first SSE event not delivered before upstream finished; response is being buffered")
	}
	if fe.err != nil {
		t.Fatal(fe.err)
	}
	defer fe.resp.Body.Close()
	if fe.event != first {
		t.Fatalf("first event = %q, want %q", fe.event, first)
	}

	unblock()
	br := fe.br
	if got := readAll(t, br); got != rest {
		t.Errorf("remaining stream = %q, want %q", got, rest)
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
	for i := 0; i < len(lmStudioV1Stream); i++ {
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

// Fixtures in testdata/ are real responses captured from LM Studio and Ollama.
func TestCaptureFixtures(t *testing.T) {
	tests := []struct {
		file        string
		contentType string
		model       string
		tps         float64
		ttft        time.Duration
	}{
		{"lm_v1_chat.json", "application/json", "qwen/qwen3-vl-4b", 65.5780706931602, 51323 * time.Microsecond},
		{"lm_v1_chat_stream.sse", "text/event-stream", "qwen/qwen3-vl-4b", 76.20208793720948, 13606 * time.Microsecond},
		{"lm_oai.json", "application/json", "qwen/qwen3-vl-4b", 0, 0},
		{"lm_oai_stream.sse", "text/event-stream", "qwen/qwen3-vl-4b", 0, 0},
		{"ol_chat.json", "application/json; charset=utf-8", "qwen3:4b-instruct", 2 / 0.036687, 0},
		{"ol_chat_stream.ndjson", "application/x-ndjson", "qwen3:4b-instruct", 2 / 0.025315, 0},
		{"ol_oai_stream.sse", "text/event-stream", "qwen3:4b-instruct", 86.95652173913044, 0},
	}
	for _, tt := range tests {
		body, err := os.ReadFile("testdata/" + tt.file)
		if err != nil {
			t.Fatal(err)
		}
		// Feed the body whole and byte by byte to exercise chunk boundaries.
		for _, chunkSize := range []int{len(body), 1} {
			c := newCapture(tt.contentType)
			if c == nil {
				t.Fatalf("%s: no capture for %q", tt.file, tt.contentType)
			}
			for b := body; len(b) > 0; {
				n := min(chunkSize, len(b))
				c.Write(b[:n])
				b = b[n:]
			}
			got := c.finish()
			// Duration 0: totals are filled in but no throughput is estimated.
			got.completeDerived(0)
			if got.ModelName != tt.model || got.PromptTokens != 13 || got.CompletionTokens != 2 ||
				got.TotalTokens != 15 || !approxEqual(got.TokensPerSecond, tt.tps) || got.TimeToFirstToken != tt.ttft {
				t.Errorf("%s (chunk %d): got %+v, want model=%q tokens=13+2 tps=%v ttft=%v",
					tt.file, chunkSize, got, tt.model, tt.tps, tt.ttft)
			}
		}
	}
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

func TestProxyOllamaNativeNDJSON(t *testing.T) {
	stream, err := os.ReadFile("testdata/ol_chat_stream.ndjson")
	if err != nil {
		t.Fatal(err)
	}
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
	if metrics.ModelName != "qwen3:4b-instruct" || metrics.TotalTokens != 15 || !approxEqual(metrics.TokensPerSecond, 2/0.025315) {
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

func TestNonAPIPathNotForwarded(t *testing.T) {
	var hits atomic.Int32
	target := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	srv, _ := startProxy(t, target)

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

func TestConcurrentRequests(t *testing.T) {
	target := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, chatCompletionJSON)
	}))
	srv, _ := startProxy(t, target)

	const n = 25
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{}`))
			if err != nil {
				t.Error(err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
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
	}{
		{"reported rate kept", MetricsData{CompletionTokens: 10, TokensPerSecond: 7, Duration: time.Second}, 0, 10, 7},
		{"ttft excluded", MetricsData{PromptTokens: 5, CompletionTokens: 10, TimeToFirstToken: time.Second, Duration: 3 * time.Second}, 0, 15, 5},
		{"stream time used", MetricsData{CompletionTokens: 10, Duration: 3 * time.Second}, 2 * time.Second, 10, 5},
		{"whole duration fallback", MetricsData{CompletionTokens: 10, Duration: 2 * time.Second}, 0, 10, 5},
		{"no completion tokens", MetricsData{PromptTokens: 4, Duration: time.Second}, 0, 4, 0},
	}
	for _, tt := range tests {
		d := tt.in
		d.completeDerived(tt.streamTime)
		if d.TotalTokens != tt.wantTotal || d.TokensPerSecond != tt.wantTPS {
			t.Errorf("%s: total %d, tps %v; want %d, %v", tt.name, d.TotalTokens, d.TokensPerSecond, tt.wantTotal, tt.wantTPS)
		}
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
