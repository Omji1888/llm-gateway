# llm-gateway

A Go gateway that sits between apps and the Gemini API.

**Done:** proxy endpoint, 15s timeout, request logging, token bucket rate limiter
**Planned:** circuit breaker with fallback routing, semantic cache

## Results
- Rate limiter: 8 rapid requests -> 3 accepted, 5 rejected (HTTP 429) in under 1ms each
- Gemini round-trip latency: ~0.8-1.0s per request (1.7s cold)
- Rejected requests never reach the upstream API, so they cost nothing
