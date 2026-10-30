package openai_compat

// M2-6 gateway observability: Prometheus-text metrics without the client
// library. go.mod keeps client_golang as an indirect-only dependency, so the
// registry hand-rolls counters and cumulative histograms and renders the
// standard exposition format (HELP/TYPE + samples) at GET /v1/metrics,
// mounted behind the same GatewayAuth as the rest of the gateway.
//
// Metric families (labels bounded by protocol/agent/outcome dimensions):
//
//	gateway_requests_total{protocol,outcome}            requests by result class
//	gateway_model_not_found_total{agent}                404 model_not_found (unpublished/not granted)
//	gateway_budget_exceeded_total{limit}                429 budget_exceeded by limit scope
//	gateway_concurrency_limited_total                   429 concurrency_limit
//	gateway_translation_dropped_total{kind}             M2 event-translation drop volume
//	gateway_tokens_total{direction}                     prompt/completion token sums
//	gateway_ttfb_seconds{protocol}                      time to first content frame (streams)
//	gateway_turn_duration_seconds{protocol}             end-to-end turn latency
//	gateway_internal_rounds{protocol}                   internal LLM/tool rounds per request
//	gateway_token_amplification_ratio{protocol}         internal total tokens / visible answer tokens
//
// amplification compares the internal agent-loop token spend against the
// tokens the client actually sees (answer, ~4 bytes/token estimate): it
// exposes how much work retrieval/tool rounds add on top of a plain chat.

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

// metricsBucketBounds are the shared histogram bucket bounds (seconds for
// latency families, ratios for amplification, counts for rounds).
var metricsBucketBounds = []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}

// metricsCount is an atomic counter cell.
type metricsCount struct {
	value atomic.Uint64
}

// reset zeroes the counter (resetGatewayMetrics test isolation).
func (c *metricsCount) reset() { c.value.Store(0) }

// metricsBucketCount is fixed so the histogram needs no slice allocation.
const metricsBucketCount = 10

// metricsHistogram is one labeled histogram: cumulative buckets + sums.
type metricsHistogram struct {
	buckets [metricsBucketCount]atomic.Uint64
	sumNano atomic.Uint64
	count   atomic.Uint64
}

func (h *metricsHistogram) observe(v float64) {
	h.count.Add(1)
	h.sumNano.Add(uint64(v * 1e9))
	for i, bound := range metricsBucketBounds {
		if i >= metricsBucketCount {
			break
		}
		if v <= bound {
			h.buckets[i].Add(1)
		}
	}
}

// gatewayMetrics is the process-wide gateway registry. Metric families are
// fixed at startup; label combinations are created lazily under a mutex and
// observed lock-free afterwards.
type gatewayMetricsRegistry struct {
	mu sync.Mutex

	requests    map[string]*metricsCount // protocol|outcome
	notFound    map[string]*metricsCount // agent
	budget      map[string]*metricsCount // limit scope
	dropped     map[string]*metricsCount // translation drop kind
	tokens      map[string]*metricsCount // direction
	concurrency metricsCount

	ttfb        map[string]*metricsHistogram // protocol
	turnSeconds map[string]*metricsHistogram // protocol
	rounds      map[string]*metricsHistogram // protocol
	amplify     map[string]*metricsHistogram // protocol
}

// gatewayMetrics is the singleton the handler stack talks to; tests may read
// it through resetGatewayMetrics.
var gatewayMetrics = newGatewayMetricsRegistry()

func newGatewayMetricsRegistry() *gatewayMetricsRegistry {
	return &gatewayMetricsRegistry{
		requests:    map[string]*metricsCount{},
		notFound:    map[string]*metricsCount{},
		budget:      map[string]*metricsCount{},
		dropped:     map[string]*metricsCount{},
		tokens:      map[string]*metricsCount{},
		ttfb:        map[string]*metricsHistogram{},
		turnSeconds: map[string]*metricsHistogram{},
		rounds:      map[string]*metricsHistogram{},
		amplify:     map[string]*metricsHistogram{},
	}
}

// resetGatewayMetrics clears every family (test isolation).
func resetGatewayMetrics() {
	fresh := newGatewayMetricsRegistry()
	gatewayMetrics.mu.Lock()
	gatewayMetrics.requests = fresh.requests
	gatewayMetrics.notFound = fresh.notFound
	gatewayMetrics.budget = fresh.budget
	gatewayMetrics.dropped = fresh.dropped
	gatewayMetrics.tokens = fresh.tokens
	// metricsCount embeds atomics; reset via its own reset helper.
	gatewayMetrics.concurrency.reset()
	gatewayMetrics.ttfb = fresh.ttfb
	gatewayMetrics.turnSeconds = fresh.turnSeconds
	gatewayMetrics.rounds = fresh.rounds
	gatewayMetrics.amplify = fresh.amplify
	gatewayMetrics.mu.Unlock()
}

func (r *gatewayMetricsRegistry) counter(m map[string]*metricsCount, key string) *metricsCount {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := m[key]; ok {
		return c
	}
	c := &metricsCount{}
	m[key] = c
	return c
}

func (r *gatewayMetricsRegistry) histogram(m map[string]*metricsHistogram, key string) *metricsHistogram {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := m[key]; ok {
		return h
	}
	h := &metricsHistogram{}
	m[key] = h
	return h
}

// --- observation helpers (called from the handler stack) ---

// metricOutcomeClass maps an HTTP status to the request outcome label.
func metricOutcomeClass(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "ok"
	case status == http.StatusNotFound:
		return "not_found"
	case status == http.StatusConflict:
		return "conflict"
	case status == http.StatusTooManyRequests:
		return "rate_limited"
	case status >= 400 && status < 500:
		return "client_error"
	case status == http.StatusRequestTimeout:
		return "timeout"
	default:
		return "server_error"
	}
}

// metricProtocolForPath classifies the shell by route path (the metrics
// middleware runs before the handler knows the protocol).
func metricProtocolForPath(path string) string {
	if strings.HasSuffix(path, "/messages") {
		return protoAnthropic
	}
	return protoOpenAI
}

// observeRequest counts one gateway request by protocol + outcome.
func (r *gatewayMetricsRegistry) observeRequest(protocol string, status int) {
	r.counter(r.requests, protocol+"|"+metricOutcomeClass(status)).value.Add(1)
}

func (r *gatewayMetricsRegistry) observeModelNotFound(agent string) {
	r.counter(r.notFound, agent).value.Add(1)
}

func (r *gatewayMetricsRegistry) observeBudgetExceeded(limit string) {
	r.counter(r.budget, limit).value.Add(1)
}

func (r *gatewayMetricsRegistry) observeConcurrencyLimited() {
	r.concurrency.value.Add(1)
}

// translationDropKinds label the translator's drop paths.
const (
	dropThoughtDropped  = "thought_dropped"
	dropThoughtComplete = "thought_completed_dropped"
	dropProgressDropped = "progress_dropped"
	dropToolMerged      = "tool_events_merged"
)

func (r *gatewayMetricsRegistry) observeTranslationDrop(kind string) {
	r.counter(r.dropped, kind).value.Add(1)
}

func (r *gatewayMetricsRegistry) observeTokens(direction string, tokens int64) {
	if tokens <= 0 {
		return
	}
	r.counter(r.tokens, direction).value.Add(uint64(tokens))
}

func (r *gatewayMetricsRegistry) observeTTFB(protocol, agent string, d time.Duration) {
	key := protocol
	if agent != "" {
		key += "|" + agent
	}
	r.histogram(r.ttfb, key).observe(d.Seconds())
}

func (r *gatewayMetricsRegistry) observeTurnDuration(protocol string, d time.Duration) {
	r.histogram(r.turnSeconds, protocol).observe(d.Seconds())
}

func (r *gatewayMetricsRegistry) observeInternalRounds(protocol string, rounds int) {
	if rounds <= 0 {
		return
	}
	r.histogram(r.rounds, protocol).observe(float64(rounds))
}

// observeAmplification records totalTokens / visibleAnswerTokens (answer
// runes / 4 ≈ tokens). A non-positive answer degrades to 0 and is skipped.
func (r *gatewayMetricsRegistry) observeAmplification(protocol string, totalTokens int64, answer string) {
	if totalTokens <= 0 {
		return
	}
	const runesPerToken = 4
	visible := int64(len([]rune(answer)) / runesPerToken)
	if visible <= 0 {
		return
	}
	r.histogram(r.amplify, protocol).observe(float64(totalTokens) / float64(visible))
}

// --- exposition rendering ---

// renderGatewayMetrics emits the Prometheus text exposition format.
func renderGatewayMetrics(r *gatewayMetricsRegistry) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var sb strings.Builder

	writeFamily := func(name, help, typ string) {
		fmt.Fprintf(&sb, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}

	writeFamily("gateway_requests_total", "Gateway requests by protocol and outcome class.", "counter")
	for _, key := range sortedKeys(r.requests) {
		parts := strings.SplitN(key, "|", 2)
		fmt.Fprintf(&sb, "gateway_requests_total{protocol=%q,outcome=%q} %d\n",
			parts[0], parts[1], r.requests[key].value.Load())
	}

	writeFamily("gateway_model_not_found_total", "Requests rejected with 404 model_not_found, by agent.", "counter")
	for _, key := range sortedKeys(r.notFound) {
		fmt.Fprintf(&sb, "gateway_model_not_found_total{agent=%q} %d\n", key, r.notFound[key].value.Load())
	}

	writeFamily("gateway_budget_exceeded_total", "Requests rejected with 429 budget_exceeded, by limit scope.", "counter")
	for _, key := range sortedKeys(r.budget) {
		fmt.Fprintf(&sb, "gateway_budget_exceeded_total{limit=%q} %d\n", key, r.budget[key].value.Load())
	}

	writeFamily("gateway_concurrency_limited_total", "Requests rejected with 429 concurrency_limit.", "counter")
	fmt.Fprintf(&sb, "gateway_concurrency_limited_total %d\n", r.concurrency.value.Load())

	writeFamily("gateway_translation_dropped_total", "Agent events dropped or merged by the protocol translator.", "counter")
	for _, key := range sortedKeys(r.dropped) {
		fmt.Fprintf(&sb, "gateway_translation_dropped_total{kind=%q} %d\n", key, r.dropped[key].value.Load())
	}

	writeFamily("gateway_tokens_total", "Gateway token usage totals by direction.", "counter")
	for _, key := range sortedKeys(r.tokens) {
		fmt.Fprintf(&sb, "gateway_tokens_total{direction=%q} %d\n", key, r.tokens[key].value.Load())
	}

	writeHistogramFamily := func(name, help string, m map[string]*metricsHistogram, unit string) {
		writeFamily(name, help, "histogram")
		for _, key := range sortedKeys(m) {
			h := m[key]
			labels := histogramLabels(key, unit)
			var cumulative uint64
			for i, bound := range metricsBucketBounds {
				if i >= metricsBucketCount {
					break
				}
				cumulative = h.buckets[i].Load()
				fmt.Fprintf(&sb, "%s_bucket%s,le=\"%s\"} %d\n", name, labels, formatFloat(bound), cumulative)
			}
			fmt.Fprintf(&sb, "%s_bucket%s,le=\"+Inf\"} %d\n", name, labels, h.count.Load())
			fmt.Fprintf(&sb, "%s_sum%s} %s\n", name, labels, formatNano(h.sumNano.Load()))
			fmt.Fprintf(&sb, "%s_count%s} %d\n", name, labels, h.count.Load())
		}
	}
	writeHistogramFamily("gateway_ttfb_seconds",
		"Time to first content frame on streaming turns.", r.ttfb, "protocol,agent")
	writeHistogramFamily("gateway_turn_duration_seconds",
		"End-to-end gateway turn duration.", r.turnSeconds, "protocol")
	writeHistogramFamily("gateway_internal_rounds",
		"Internal agent-loop rounds per request.", r.rounds, "protocol")
	writeHistogramFamily("gateway_token_amplification_ratio",
		"Internal total tokens divided by visible answer tokens.", r.amplify, "protocol")

	return sb.String()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// histogramLabels renders the fixed label set of a histogram series (the
// trailing brace is closed by the sample suffix).
func histogramLabels(key, unit string) string {
	switch unit {
	case "protocol,agent":
		parts := strings.SplitN(key, "|", 2)
		agent := "unknown"
		if len(parts) == 2 {
			agent = parts[1]
		}
		return fmt.Sprintf("{protocol=%q,agent=%q", parts[0], agent)
	default:
		return fmt.Sprintf("{protocol=%q", key)
	}
}

func formatFloat(v float64) string {
	return fmt.Sprintf("%g", v)
}

func formatNano(nano uint64) string {
	return fmt.Sprintf("%g", float64(nano)/1e9)
}

// observeTurnMetrics records the end-of-turn families: duration (always)
// and token amplification (when usage + visible answer exist).
func observeTurnMetrics(
	protocol string, usage *Usage, telemetry *turnTelemetry, started time.Time,
) {
	gatewayMetrics.observeTurnDuration(protocol, time.Since(started))
	if usage != nil {
		gatewayMetrics.observeTokens("prompt", usage.PromptTokens)
		gatewayMetrics.observeTokens("completion", usage.CompletionTokens)
		if telemetry != nil && telemetry.AnswerRunes > 0 {
			answer := strings.Repeat("x", telemetry.AnswerRunes)
			gatewayMetrics.observeAmplification(protocol, usage.TotalTokens, answer)
		}
	}
	if telemetry != nil && telemetry.Rounds > 0 {
		gatewayMetrics.observeInternalRounds(protocol, telemetry.Rounds)
	}
}

// --- gin glue ---

// GatewayMetricsMiddleware counts every request on the gateway group by
// protocol (path-derived) and outcome (response status). Package-level so
// the router can mount it before the routes.
var GatewayMetricsMiddleware = func(c *gin.Context) {
	protocol := metricProtocolForPath(c.Request.URL.Path)
	c.Next()
	gatewayMetrics.observeRequest(protocol, c.Writer.Status())
}

// GatewayMetrics serves the exposition endpoint behind GatewayAuth.
//
// @Summary      网关指标
// @Description  Prometheus 文本格式的网关运行指标(请求计数、TTFB、内部轮数、token 放大比、翻译丢弃量、429/404 计数)
// @Tags         网关(可观测性)
// @Produce      text/plain
// @Success      200 {string} string "Prometheus exposition"
// @Security     ApiKeyAuth
// @Router       /v1/metrics [get]
func (h *GatewayHandler) GatewayMetrics(c *gin.Context) {
	c.Header("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	c.String(http.StatusOK, renderGatewayMetrics(gatewayMetrics))
}
