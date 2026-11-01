package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"
)

const url = "http://127.0.0.1:8080/chat"

var client = &http.Client{Timeout: 30 * time.Second}

type result struct {
	status  int
	cache   string
	latency time.Duration
}

func send(prompt string) result {
	body := fmt.Sprintf(`{"prompt":%q}`, prompt)
	start := time.Now()
	resp, err := client.Post(url, "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		return result{status: 0, latency: time.Since(start)}
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return result{status: resp.StatusCode, cache: resp.Header.Get("X-Cache"), latency: time.Since(start)}
}

func pct(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d[int(float64(len(d)-1)*p)]
}

func avg(d []time.Duration) time.Duration {
	if len(d) == 0 {
		return 0
	}
	var sum time.Duration
	for _, x := range d {
		sum += x
	}
	return sum / time.Duration(len(d))
}

func burst() {
	const total, workers = 200, 20
	jobs := make(chan struct{}, total)
	for i := 0; i < total; i++ {
		jobs <- struct{}{}
	}
	close(jobs)

	var mu sync.Mutex
	var results []result
	var wg sync.WaitGroup

	start := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range jobs {
				r := send("")
				mu.Lock()
				results = append(results, r)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	counts := map[int]int{}
	var lats []time.Duration
	for _, r := range results {
		counts[r.status]++
		lats = append(lats, r.latency)
	}

	fmt.Println("=== BURST TEST ===")
	fmt.Printf("requests: %d, concurrency: %d, took: %v\n", total, workers, elapsed.Round(time.Millisecond))
	fmt.Printf("throughput: %.0f req/s\n", float64(total)/elapsed.Seconds())
	fmt.Printf("passed limiter (400): %d\n", counts[400])
	fmt.Printf("blocked (429): %d\n", counts[429])
	fmt.Printf("errors (0): %d\n", counts[0])
	fmt.Printf("latency p50=%v p95=%v p99=%v\n",
		pct(lats, 0.50).Round(time.Microsecond),
		pct(lats, 0.95).Round(time.Microsecond),
		pct(lats, 0.99).Round(time.Microsecond))
}

func cacheTest() {
	pairs := [][2]string{
		{"What is the capital of France?", "Tell me the capital city of France"},
		{"How do I boil an egg?", "What is the way to boil an egg?"},
		{"Who wrote Romeo and Juliet?", "Which author wrote Romeo and Juliet?"},
		{"What is photosynthesis?", "Can you explain what photosynthesis is?"},
		{"How many days are in a leap year?", "How many days does a leap year have?"},
	}

	var hitLat, missLat []time.Duration
	hits := 0

	run := func(label, p string) {
		r := send(p)
		fmt.Printf("%-5s %-42s -> %d cache=%-4s %v\n", label, p, r.status, r.cache, r.latency.Round(time.Millisecond))
		if r.cache == "HIT" {
			hitLat = append(hitLat, r.latency)
			if label == "para" {
				hits++
			}
		} else if r.cache == "MISS" {
			missLat = append(missLat, r.latency)
		}
		time.Sleep(1100 * time.Millisecond) // stay under the rate limit
	}

	fmt.Println("=== CACHE TEST ===")
	for _, p := range pairs {
		run("orig", p[0])
	}
	for _, p := range pairs {
		run("para", p[1])
	}

	fmt.Printf("\nreworded prompts served from cache: %d/%d\n", hits, len(pairs))
	fmt.Printf("avg MISS latency: %v\n", avg(missLat).Round(time.Millisecond))
	fmt.Printf("avg HIT latency:  %v\n", avg(hitLat).Round(time.Millisecond))
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: go run ./loadtest burst | cache")
		return
	}
	switch os.Args[1] {
	case "burst":
		burst()
	case "cache":
		cacheTest()
	default:
		fmt.Println("unknown mode:", os.Args[1])
	}
}
