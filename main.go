package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

const primaryModel = "gemini-3.8-flash"
const fallbackModel = "gemini-3.5-flash-lite"

var client = &http.Client{Timeout: 15 * time.Second}
var primaryClient = &http.Client{Timeout: 6 * time.Second}

type chatReq struct {
	Prompt string `json:"prompt"`
}

// ---------- logging ----------

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func logging(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next(rec, r)
		log.Printf("%s %s -> %d (%v)", r.Method, r.URL.Path, rec.status, time.Since(start))
	}
}

// ---------- rate limiter (token bucket) ----------

type bucket struct {
	tokens float64
	last   time.Time
}

type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64
	burst   float64
}

func newLimiter(rate, burst float64) *limiter {
	return &limiter{buckets: make(map[string]*bucket), rate: rate, burst: burst}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}

	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now

	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

func rateLimit(l *limiter, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ip = r.RemoteAddr
		}
		if !l.allow(ip) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}

// ---------- circuit breaker ----------

type breaker struct {
	mu        sync.Mutex
	failures  int
	openUntil time.Time
	threshold int
	cooldown  time.Duration
}

func newBreaker(threshold int, cooldown time.Duration) *breaker {
	return &breaker{threshold: threshold, cooldown: cooldown}
}

func (b *breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !time.Now().Before(b.openUntil)
}

func (b *breaker) record(success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if success {
		b.failures = 0
		return
	}
	b.failures++
	if b.failures >= b.threshold {
		b.openUntil = time.Now().Add(b.cooldown)
	}
}

var breakers = map[string]*breaker{
	primaryModel:  newBreaker(3, 30*time.Second),
	fallbackModel: newBreaker(3, 30*time.Second),
}

// ---------- semantic cache ----------

const embedModel = "gemini-embedding-001"
const cacheThreshold = 0.90
const cacheMax = 500
const cacheTTL = time.Hour

type cacheEntry struct {
	embedding []float64
	response  []byte
	model     string
	expires   time.Time
}

type semanticCache struct {
	mu      sync.Mutex
	entries []cacheEntry
}

var cache = &semanticCache{}

func embed(text string) ([]float64, error) {
	body, _ := json.Marshal(map[string]any{
		"model":    "models/" + embedModel,
		"taskType": "SEMANTIC_SIMILARITY",
		"content": map[string]any{
			"parts": []any{map[string]any{"text": text}},
		},
	})
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:embedContent", embedModel)
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", os.Getenv("GEMINI_API_KEY"))

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("embed status %d", resp.StatusCode)
	}

	var out struct {
		Embedding struct {
			Values []float64 `json:"values"`
		} `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Embedding.Values, nil
}

func cosine(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func (c *semanticCache) lookup(emb []float64) (cacheEntry, float64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	best := -1.0
	var bestEntry cacheEntry
	for _, e := range c.entries {
		if now.After(e.expires) {
			continue
		}
		s := cosine(emb, e.embedding)
		if s > best {
			best = s
			bestEntry = e
		}
	}
	return bestEntry, best, best >= cacheThreshold
}

func (c *semanticCache) store(e cacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= cacheMax {
		c.entries = c.entries[1:]
	}
	c.entries = append(c.entries, e)
}

// ---------- calling Gemini ----------

func callModel(model string, body []byte) (*http.Response, error) {
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent", model)
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", os.Getenv("GEMINI_API_KEY"))

	c := client
	if model == primaryModel {
		c = primaryClient
	}
	return c.Do(req)
}

func isFailure(resp *http.Response, err error) bool {
	if err != nil {
		return true
	}
	c := resp.StatusCode
	return c == 404 || c == 429 || c >= 500
}

// ---------- chat handler ----------

func chatHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req chatReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Prompt == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	emb, embErr := embed(req.Prompt)
	if embErr != nil {
		log.Println("embed error (cache skipped):", embErr)
	} else {
		e, sim, hit := cache.lookup(emb)
		if hit {
			log.Printf("cache HIT sim=%.3f", sim)
			w.Header().Set("X-Cache", "HIT")
			w.Header().Set("X-Model-Used", e.model)
			w.Header().Set("Content-Type", "application/json")
			w.Write(e.response)
			return
		}
		if sim >= 0 {
			log.Printf("cache MISS best sim=%.3f", sim)
		}
	}

	body, _ := json.Marshal(map[string]any{
		"contents": []any{
			map[string]any{"parts": []any{map[string]any{"text": req.Prompt}}},
		},
	})

	for _, m := range []string{primaryModel, fallbackModel} {
		br := breakers[m]
		if !br.allow() {
			log.Printf("breaker open for %s, skipping", m)
			continue
		}

		resp, err := callModel(m, body)
		if isFailure(resp, err) {
			br.record(false)
			log.Printf("model %s failed (err=%v), trying next", m, err)
			if resp != nil {
				resp.Body.Close()
			}
			continue
		}

		br.record(true)
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == 200 && embErr == nil {
			cache.store(cacheEntry{
				embedding: emb,
				response:  data,
				model:     m,
				expires:   time.Now().Add(cacheTTL),
			})
		}

		w.Header().Set("X-Cache", "MISS")
		w.Header().Set("X-Model-Used", m)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(data)
		return
	}

	http.Error(w, "all models unavailable", http.StatusServiceUnavailable)
}

func main() {
	if os.Getenv("GEMINI_API_KEY") == "" {
		log.Fatal("GEMINI_API_KEY is not set")
	}
	lim := newLimiter(1, 3)
	http.HandleFunc("/chat", logging(rateLimit(lim, chatHandler)))
	log.Println("listening on 127.0.0.1:8080")
	log.Fatal(http.ListenAndServe("127.0.0.1:8080", nil))
}
