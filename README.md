# 🛡️ Telemetry-Enabled API Gateway with Distributed Rate Limiting

A highly-performant, telemetry-enabled API Gateway built in Go. This repository implements path-based reverse routing, stateless cryptographic authentication (JWT), and real-time distributed rate limiting powered by an atomic Redis-based **Token Bucket** algorithm.

---

## 🏗️ Architecture Flow

The following diagram illustrates the lifecycle of an incoming request as it flows through the API Gateway, validating security credentials, evaluating rate limits, injecting telemetry, and forwarding the request to the upstream microservices:

```
                  [ Client Request ]
                         │
                         ▼
┌────────────────────────────────────────────────────────┐
│               1. API Gateway Router                    │
│   (Resolves target upstream prefix matching via YAML)   │
└───────────────────────┬────────────────────────────────┘
                        │
                        ├──────────────────────────┐
                        │ [Route Unmapped]         │ [Route Matched]
                        ▼                          ▼
              [ 404 Route Unmapped ]     ┌───────────────────┐
                                         │ 2. JWT Validator  │
                                         │ (Verify HS256 JWT │
                                         │   Bearer Token)   │
                                         └─────────┬─────────┘
                                                   │
                                     ┌─────────────┴─────────────┐
                                     │ [Invalid Signature/Exp]   │ [Claims Extracted]
                                     ▼                           ▼
                           [ 401 Unauthorized ]       ┌─────────────────────┐
                                                      │ 3. Tier Resolver    │
                                                      │ (Extract Sub / Tier │
                                                      │  e.g., Premium/Free)│
                                                      └──────────┬──────────┘
                                                                 │
                                                                 ▼
                                                      ┌─────────────────────┐
                                                      │ 4. Rate Limiter     │
                                                      │ (Atomic Lua Script  │
                                                      │  on Redis Bucket)   │
                                                      └──────────┬──────────┘
                                                                 │
                                     ┌───────────────────────────┴───────────────────────────┐
                                     │ [Bucket Depleted (Allowed = False)]                   │ [Tokens Available (Allowed = True)]
                                     ▼                                                       ▼
                         ┌───────────────────────┐                               ┌───────────────────────┐
                         │   5a. Block Request   │                               │ 5b. Pass-through Handoff│
                         │ - Inject telemetry:   │                               │ - Inject telemetry:   │
                         │   X-RateLimit-Limit   │                               │   X-RateLimit-Limit   │
                         │   X-RateLimit-Remaining│                              │   X-RateLimit-Remaining│
                         │   Retry-After: 1      │                               │ - ServeHTTP Reverse   │
                         │ - Return 429 status   │                               │   Proxy to Upstream   │
                         └───────────────────────┘                               └───────────────────────┘
```

---

## 💡 System Design Principles & Key Concepts

This project showcases production-grade cloud architectural concepts and systems engineering designs:

### 1. API Gateway Pattern
Instead of exposing internal microservices directly to the public internet, the **API Gateway** acts as a reverse proxy, acting as the single entry point. It isolates backend topology, handles edge validation (Routing, Auth, and Rate Limiting), and intercepts traffic, reducing latency overhead and security footprints on upstream application servers.

### 2. Distributed Rate Limiting via Token Bucket Algorithm
Rather than simple window counters (which suffer from bursts at window boundaries), this gate uses the **Token Bucket Algorithm**. 
- Each user key is allocated a maximum capacity of "tokens".
- Tokens are continuously replenished over time at a stable **Fill Rate** (e.g., $N$ tokens per second) up to the bucket's maximum capacity.
- Every incoming request consumes 1 token.
- This allows clients to handle **bursty traffic** up to their maximum capacity while strictly shaping average network load to the fill rate limit.

### 3. Concurrency Safety & Atomicity via Redis Lua Scripting
In a distributed environment with multiple gateway replicas or concurrent HTTP connections, standard database read-then-write pipelines suffer from **Race Conditions** (e.g., two requests checking the remaining tokens at the exact same millisecond, both seeing `1` token left, allowing both, and dropping the bucket count to `-1`).
- This gateway solves this by using a **Lua Script executed atomically** inside Redis.
- Since Redis is single-threaded, the entire script is executed sequentially on the database engine.
- This guarantees complete atomicity, minimizes lock overhead, and reduces network roundtrip latency to a single roundtrip.

### 4. Stateless Cryptographic Authentication (JWT)
To scale authentication without querying a relational database on every single incoming packet, the gateway utilizes **JSON Web Tokens (JWT)**.
- Authentication tokens are cryptographically signed using symmetric HMAC-SHA256 keys.
- The gateway validates token authenticity locally by verifying the signature.
- Client context parameters such as the **Subject (UserID)** and **Subscription Tier (Premium/Free)** are safely parsed directly from the validated token payload, bypassing authorization databases entirely.

### 5. Telemetry & RFC-Compliant HTTP Handshaking
- **Self-Documenting Headers:** Every evaluation updates client states and appends HTTP response headers showing `X-RateLimit-Limit` and `X-RateLimit-Remaining`.
- **Active Backoff Steering:** When a threshold is violated, the gateway returns a strict standard standard-compliant HTTP status `429 Too Many Requests` alongside a `Retry-After: 1` header, prompting polite client clients to backoff for 1 second.

---

## 🗂️ Project Directory Map

```text
├── gateway.yaml            # Declarative config mapping ports, routing, and tier rules
├── generatetoken.go        # Utility tool creating sample Premium and Free subscriber tokens
├── go.mod                  # Package management and dependency control
├── go.sum                  # Package integrity lock file
├── limiter/
│   └── limiter.go          # Token Bucket evaluator utilizing the atomic Redis Lua Engine
├── main.go                 # Gateway bootstrap, middleware router, proxy, and auth system
├── test.sh                 # Testing utility executing curl loops for the Premium tier
└── test_premium.sh         # Helper verifying rapid concurrent request rejection thresholds
```

---

## ⚙️ Configuration (`gateway.yaml`)

Configure server parameters, route mapping endpoints, and subscription tier rules dynamically:

```yaml
server:
  port: "8080"

# Centralized Tier Definitions
tiers:
  premium:
    capacity: 20
    fill_rate: 5      # 5 tokens added per second
  free:
    capacity: 5
    fill_rate: 1      # 1 token added per second

# Downstream routing prefix configuration
routes:
  - path: "/api/v1/products"
    target: "http://localhost:8081"
  - path: "/api/v1/checkout"
    target: "http://localhost:8082"
```

---

## 🚀 Getting Started

### Prerequisites
* **Go** (Version 1.25.0 or later recommended)
* **Redis Server** (listening on standard port `6379`)

### 1. Launch Redis
Ensure Redis is running locally:
```bash
redis-server
```

### 2. Generate Security Tokens
Build and run the cryptographic token generator tool to obtain test tokens for both `Free` and `Premium` tiers:
```bash
go run generatetoken.go
```
*This will print signed JWT tokens to your console.*

### 3. Spin up the API Gateway
Bootstrap the central gateway server:
```bash
go run main.go
```
The console will log target maps and confirm that it is listening:
```text
Shield Armed: Route Prefix /api/v1/products ➡️ http://localhost:8081
Shield Armed: Route Prefix /api/v1/checkout ➡️ http://localhost:8082
Telemetry-Enabled API Gateway listening on port :8080
```

### 4. Verify Rate-Limiting & Proxy Operations
Use the test scripts or run curl requests yourself to inspect response headers:
```bash
curl -i -H "Authorization: Bearer <TOKEN>" http://localhost:8080/api/v1/products
```

Expected output headers:
```http
HTTP/1.1 200 OK
X-Ratelimit-Limit: 20
X-Ratelimit-Remaining: 19
...
```

If you spam the requests beyond your tier allocation, the gateway protects your resources immediately:
```http
HTTP/1.1 429 Too Many Requests
Retry-After: 1
X-Ratelimit-Limit: 5
X-Ratelimit-Remaining: 0
Content-Type: text/plain; charset=utf-8

429 Too Many Requests - Your free limit allocation is completely empty.
```

---

## 🛠️ Technology Stack
* **Language:** Go
* **Datastore:** Redis (used for high-performance memory storage & atomic Lua scripting)
* **Packages Used:** 
  * `github.com/redis/go-redis/v9` (Redis client interface)
  * `github.com/golang-jwt/jwt/v5` (Stateless JSON Web Tokens)
  * `gopkg.in/yaml.v3` (Declarative YAML configuration parser)
