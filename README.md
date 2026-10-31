# llm-gateway
A Go gateway that sits between apps and the Gemini API.

Done: proxy endpoint, 15s timeout, request logging
Planned: rate limiter, circuit breaker + fallback routing, semantic cache# llm-gateway


Day 3
Everything on your PC arrives from 127.0.0.1, so all clients share one bucket. A real deployment would key by API key or real client IP.
The buckets map never shrinks, so a long-running server needs a cleanup loop.
