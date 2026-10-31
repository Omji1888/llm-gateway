package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

const primaryModel = "gemini-3.8-flash"
const fallbackModel = "gemini-3.5-flash-lite"

var client = &http.Client{Timeout: 15 * time.Second}

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

// allow returns false while the breaker is open.
// After the cooldown it lets requests through again; one more
// failure re-opens it immediately, one success resets it.
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

// ---------- calling Gemini ----------

func callModel(model string, body []byte) (*http.Response, error) {
	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent", model)
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", os.Getenv("GEMINI_API_KEY"))
	return client.Do(req)
}

// a response counts as a failure if the model is unavailable or overloaded
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
		defer resp.Body.Close()
		w.Header().Set("X-Model-Used", m)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
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
