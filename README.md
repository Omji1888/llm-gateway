# llm-gateway

A reliability layer in Go that sits between an app and the Gemini API.
Built from scratch with the standard library only (no frameworks).

## Features
- **Proxy** `POST /chat` forwarding to Gemini
- **Timeouts** 6s on the primary model, 15s on the fallback
- **Request logging** method, path, status, latency
- **Rate limiter** token bucket per client IP (1 req/s refill, burst of 3)
- **Circuit breaker + fallback** 3 consecutive failures open the breaker for 30s; traffic moves to a backup model
- **Semantic cache** embeds prompts with `gemini-embedding-001`, serves a saved answer at cosine similarity >= 0.90

## Results (measured on my laptop, Nov 2026)

**Rate limiter** (200 requests, 20 concurrent, empty prompt so Gemini is never called)
- 3 passed, 197 rejected with HTTP 429, 0 errors
- Rejection latency: p50 ~4.5ms, p95 ~10.8ms, p99 ~13.4ms
- Rejected requests never reach the upstream API, so they cost nothing

**Semantic cache** (5 questions, then 5 reworded versions)
- 5/5 reworded prompts were served from cache
- Similarity: reworded prompts scored 0.977, unrelated prompts scored 0.71-0.77, so the 0.90 threshold separates them
- Avg HIT latency 877ms (mostly the embedding call). When Gemini is healthy (~0.8-1.0s per call) this is not faster; the benefit is cost and resilience. When the primary was slow, misses averaged 6.0s.

**Circuit breaker + fallback**
- With an invalid primary model, 5/5 requests were still answered by the fallback; the breaker opened after 3 failures
- During real testing the primary timed out repeatedly. Lowering its timeout from 15s to 6s cut worst-case waiting on a failing primary from 15s to 6s
- Every request still returned a valid answer (no client-visible errors)

## Run it
    set GEMINI_API_KEY (PowerShell: $env:GEMINI_API_KEY="...")
    go run main.go
    go run ./loadtest burst
    go run ./loadtest cache

## Known limitations
- The breaker's half-open state is simplified: after the cooldown it lets all traffic through instead of sending a single probe request
- The cache and rate limiter are in memory only (lost on restart), and the rate limiter map never shrinks
- The cache scans entries linearly (fine for hundreds, not millions), and has no per-user separation
- Not safe to cache time-sensitive questions
- All local clients share one IP, so the rate limiter treats them as one client
