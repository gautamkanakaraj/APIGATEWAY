# 🛡️ Enterprise-Grade Telemetry-Enabled API Gateway

A high-performance, containerized API Gateway built in Go designed for resilience, visibility, and dynamic operations. 

This project extends a core reverse-routing and JWT authentication engine into an enterprise-grade gateway featuring **Dynamic Load Balancing**, active **Health Checks**, **Circuit Breaking**, **Prometheus/OpenTelemetry Observability**, and **Zero-Downtime Hot-Reloading**.

---

## 🏗️ Architecture & Component Flow

The API Gateway processes incoming edge requests through a series of middleware pipelines before proxying traffic downstream.

Please refer to the comprehensive [architecture.txt](architecture.txt) diagram for detailed ASCII lifecycle transitions of the request path, background health daemons, and hot-reload triggers.

### Summary Flow:
```text
[ Client Request ] ──► [ Observability Middleware ] ──► [ JWT Edge Auth ]
                                                              │
                                                     (Allowed & Authenticated)
                                                              │
                                                              ▼
[ Upstream Microservices ] ◄── [ Circuit Breaker ] ◄── [ Dynamic Load Balancer ]
```

---

## 💡 Key Enterprise Pillars

### 1. Fault Tolerance (Circuit Breaking)
Downstream service outages can cascade and consume gateway connection pools. We integrate `github.com/sony/gobreaker` directly into the routing layer:
* **Failure Capture**: A custom ResponseWriter wrapper traps downstream network connection timeouts and `5xx` response codes.
* **Cooperative Failover**: If a backend hits a 50% failure rate threshold over 5+ requests, its Circuit Breaker transitions to the **Open** state. The Dynamic Load Balancer automatically bypasses open breakers, executing seamless routing to healthy alternatives.
* **Auto-Recovery**: After a 5-second cooldown, the breaker enters a **Half-Open** state, permitting trial traffic to evaluate downstream recovery.

### 2. Observability Pipeline (Metrics & Distributed Tracing)
* **Prometheus Integration**: Middleware automatically tracks requests count and execution latencies:
  * `gateway_requests_total{method, path, status}`
  * `gateway_request_duration_seconds{method, path, status}`
  * Exposes standard telemetry endpoints at `/metrics` (bypassed from JWT authentication).
* **OpenTelemetry Distributed Tracing**: Translates, extracts, or generates transaction traces (`X-Trace-ID`). It injects transaction contexts downstream to ensure end-to-end trace correlation across all mock services.

### 3. Dynamic Operations (Zero-Downtime Hot-Reloading)
* **Thread-Safe Configurations**: Guarded by a `sync.RWMutex`, route maps and tier rules are read-locked during request resolution for split-second lookups, avoiding execution bottlenecks.
* **fsnotify Watcher**: Watches the configuration folder. Updates to `gateway.yaml` instantly trigger a configuration parse, reloading targets, rebuilding backend pools, and shutting down old health check daemons cleanly to avoid goroutine leaks.

### 4. Container Orchestration
* **Multi-Stage Dockerfile**: Compiles using `golang:1.26-alpine` and strips debug symbols (`-ldflags="-s -w"`) to output a minimal production-ready `alpine` runner (~15MB).
* **Unified Binary**: The gateway binary acts as either the gateway or a mock service depending on the environment variable `GATEWAY_MODE` (e.g. `GATEWAY_MODE=mock`), simplifying container compilation and images.
* **Docker Compose**: Spins up a full network topology of the Gateway, Redis cache, and 3 mock instances (2 load-balanced product service instances and 1 checkout instance).

---

## 🗂️ Project Directory Map

```text
├── balancer.go             # Round-Robin Load Balancer, health daemon, and Circuit Breaker
├── docker-compose.yml      # Multicontainer setup orchestrating Gateway, Redis, and mock nodes
├── Dockerfile              # Multi-stage optimized Go build
├── gateway.yaml            # Config mapping ports, load-balancing targets, and rate limits
├── generatetoken.go        # Utility creating signed Premium and Free testing tokens
├── go.mod                  # Package management and dependencies
├── go.sum                  # Package integrity lock file
├── limiter/
│   └── limiter.go          # Token Bucket evaluator utilizing the atomic Redis Lua Engine
├── main.go                 # Gateway bootstrap, fsnotify reload, and observability middleware
├── README.md               # Up-to-date documentation and quickstart guide
├── architecture.txt        # ASCII architectural diagram
├── test.sh                 # Testing utility executing curl loops for the Premium tier
└── test_premium.sh         # Helper verifying rapid concurrent request rejection thresholds
```

---

## ⚙️ Configuration (`gateway.yaml`)

Specify server ports, centralized rate limits, and load-balanced target destinations:

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

# Downstream load-balanced routes
routes:
  - path: "/api/v1/products"
    targets:
      - "http://service-products-1:8081"
      - "http://service-products-2:8083"
  - path: "/api/v1/checkout"
    targets:
      - "http://service-checkout-1:8082"
```

---

## 🚀 Getting Started

### Quickstart (Docker Compose)

The easiest way to build and verify the entire network is using Docker Compose:

1. **Start the Infrastructure**:
   ```bash
   docker-compose up --build -d
   ```
   This builds the unified binary and spins up the Gateway, Redis, and the 3 mock services.

2. **Generate Signed Tokens**:
   Create mock JWT tokens locally (requires Go installed on host):
   ```bash
   go run generatetoken.go
   ```
   *Copy the outputted Premium Bearer token.*

3. **Verify Routing & Dynamic Load Balancing**:
   Send multiple requests to the products endpoint:
   ```bash
   TOKEN="<PASTE_PREMIUM_TOKEN>"
   for i in {1..4}; do
     curl -s -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/v1/products | grep service_name
   done
   ```
   *You will see the gateway route request 1 and 3 to `products-service-instance-1` and requests 2 and 4 to `products-service-instance-2`.*

4. **Verify Observability (Metrics & Traces)**:
   Check client response headers for trace propagation:
   ```bash
   curl -i -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/v1/products
   ```
   *Verify the presence of `X-Trace-Id`.*

   Query the Prometheus endpoint:
   ```bash
   curl -s http://localhost:8080/metrics | grep gateway_
   ```

5. **Verify Zero-Downtime Hot-Reloading**:
   Edit `gateway.yaml` on your host machine to change `premium` capacity limit rules (e.g. from 20 to 100). The gateway will reload instantly:
   ```bash
   docker logs api-gateway
   ```
   *Check for the logs: `[HOT-RELOAD SUCCESS] Route maps and rate-limit tiers reloaded cleanly.`*

6. **Verify Fault Tolerance (Failover)**:
   Stop one of the product instances:
   ```bash
   docker stop mock-products-1
   ```
   Send requests to `/api/v1/products`. The gateway immediately bypasses the stopped container, routing 100% of traffic to the healthy `mock-products-2` instance without failing client requests.

---

## 🛠️ Technology Stack
* **Language:** Go 1.26
* **Datastore:** Redis
* **Core Libraries:**
  * `github.com/redis/go-redis/v9` (Distributed Lua Token Limiting)
  * `github.com/golang-jwt/jwt/v5` (Stateless Authorization Checks)
  * `github.com/fsnotify/fsnotify` (Dynamic Config Watching)
  * `github.com/sony/gobreaker` (Cascading Outage Circuit Breaker)
  * `github.com/prometheus/client_golang` (Prometheus Metrics Registry)
  * `go.opentelemetry.io/otel` (OpenTelemetry Tracing Framework)
