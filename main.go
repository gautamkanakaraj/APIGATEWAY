package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"gateway/limiter"

	"github.com/fsnotify/fsnotify"
	"github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sony/gobreaker"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"gopkg.in/yaml.v3"
)

var jwtSecret = []byte("super-secret-gateway-key")

// Observability Metrics
var (
	requestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gateway_requests_total",
			Help: "Total number of HTTP requests processed by the API Gateway",
		},
		[]string{"method", "path", "status"},
	)
	requestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "gateway_request_duration_seconds",
			Help:    "Latency of HTTP requests processed by the API Gateway",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "path", "status"},
	)
	tracer trace.Tracer
)

func init() {
	prometheus.MustRegister(requestsTotal)
	prometheus.MustRegister(requestDuration)
}

func initTracer() {
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)
	tracer = otel.Tracer("api-gateway")
}

type TierLimit struct {
	Capacity int `yaml:"capacity"`
	FillRate int `yaml:"fill_rate"`
}

type RouteConfig struct {
	Path    string   `yaml:"path"`
	Target  string   `yaml:"target"`
	Targets []string `yaml:"targets"`
}

type Config struct {
	Server struct {
		Port string `yaml:"port"`
	} `yaml:"server"`
	Tiers  map[string]TierLimit `yaml:"tiers"`
	Routes []RouteConfig        `yaml:"routes"`
}

type Gateway struct {
	mu      sync.RWMutex
	routes  map[string]*BackendPool
	tiers   map[string]TierLimit
	limiter *limiter.RateLimiter
	cancels map[string]context.CancelFunc
}

func main() {
	// If run in mock mode, execute mock server instead
	if os.Getenv("GATEWAY_MODE") == "mock" {
		runMockServer()
		return
	}

	initTracer()

	// 1. Resolve configuration path (check config/ subfolder for container bind-mounts)
	configPath := "gateway.yaml"
	if _, err := os.Stat("config/gateway.yaml"); err == nil {
		configPath = "config/gateway.yaml"
	}

	log.Printf("[CONFIG] Loading configurations from %s", configPath)
	yamlFile, err := ioutil.ReadFile(configPath)
	if err != nil {
		log.Fatalf("Config read error: %v", err)
	}

	var config Config
	if err := yaml.Unmarshal(yamlFile, &config); err != nil {
		log.Fatalf("Config parse error: %v", err)
	}

	// 2. Connect to Redis (configurable via env for container networking)
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}
	rl := limiter.NewRateLimiter(redisAddr)

	gw := &Gateway{
		routes:  make(map[string]*BackendPool),
		tiers:   config.Tiers,
		limiter: rl,
		cancels: make(map[string]context.CancelFunc),
	}

	// Load configuration initially
	gw.reloadConfig(config)

	// 3. Start background file watcher for hot-reloads
	go gw.watchConfig(configPath)

	// 4. Register Prometheus metrics handler (bypass JWT validation)
	http.Handle("/metrics", promhttp.Handler())

	// 5. Main Secure Gateway Proxy Handler with Observability middleware
	http.HandleFunc("/", observabilityMiddleware(gw.handleSecureEdgeProxy))

	fmt.Printf("Telemetry-Enabled API Gateway listening on port :%s (Redis: %s)\n", config.Server.Port, redisAddr)
	log.Fatal(http.ListenAndServe(":"+config.Server.Port, nil))
}

func (gw *Gateway) reloadConfig(config Config) {
	gw.mu.Lock()
	defer gw.mu.Unlock()

	// 1. Cancel existing health check daemons to prevent resource leaks
	for path, cancel := range gw.cancels {
		cancel()
		delete(gw.cancels, path)
	}

	// 2. Clear current routes and assign updated tiers
	gw.routes = make(map[string]*BackendPool)
	gw.tiers = config.Tiers

	// 3. Mount Backend Pools for all routes
	for _, r := range config.Routes {
		var targets []string
		if len(r.Targets) > 0 {
			targets = r.Targets
		} else if r.Target != "" {
			targets = []string{r.Target}
		}

		backends := make([]*Backend, 0)
		for _, t := range targets {
			targetURL, err := url.Parse(t)
			if err != nil {
				log.Printf("[CONFIG ERROR] Invalid target URL %s: %v", t, err)
				continue
			}
			backends = append(backends, NewBackend(targetURL))
			log.Printf("[CONFIG] Route Target Loaded: %s ➡️ %s", r.Path, t)
		}

		if len(backends) == 0 {
			log.Printf("[CONFIG WARNING] Route %s contains no valid targets. Skipping.", r.Path)
			continue
		}

		pool := &BackendPool{backends: backends}
		gw.routes[r.Path] = pool

		// Spawn health check daemon for this target pool
		ctx, cancel := context.WithCancel(context.Background())
		gw.cancels[r.Path] = cancel
		go pool.HealthCheckDaemon(ctx)

		fmt.Printf("Shield Armed: Route Prefix %s ➡️ %d Backend(s) (health checks active)\n", r.Path, len(backends))
	}
}

func (gw *Gateway) watchConfig(configPath string) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatalf("Failed to create fsnotify watcher: %v", err)
	}
	defer watcher.Close()

	// Extract parent directory and filename to resolve Docker volume limitations
	dir := "."
	filename := configPath
	if idx := strings.LastIndex(configPath, "/"); idx != -1 {
		dir = configPath[:idx]
		filename = configPath[idx+1:]
	}

	if err := watcher.Add(dir); err != nil {
		log.Fatalf("Failed to watch configuration directory %s: %v", dir, err)
	}

	log.Printf("[HOT-RELOAD] Active watcher armed on directory '%s' for config file '%s'", dir, filename)

	var lastEventTime time.Time
	cooldown := 100 * time.Millisecond

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}

			// Only trigger reload for the target configuration file
			if !strings.HasSuffix(event.Name, filename) {
				continue
			}

			// Debounce consecutive events
			if time.Since(lastEventTime) < cooldown {
				continue
			}
			lastEventTime = time.Now()

			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) || event.Has(fsnotify.Rename) {
				log.Printf("[HOT-RELOAD] File event detected: %s (%v)", event.Name, event.Op)
				time.Sleep(50 * time.Millisecond) // Let the OS finalize the write

				yamlFile, err := ioutil.ReadFile(configPath)
				if err != nil {
					log.Printf("[HOT-RELOAD ERROR] Failed to read updated config: %v", err)
					continue
				}

				var newConfig Config
				if err := yaml.Unmarshal(yamlFile, &newConfig); err != nil {
					log.Printf("[HOT-RELOAD ERROR] Failed to parse updated yaml: %v", err)
					continue
				}

				gw.reloadConfig(newConfig)
				log.Printf("[HOT-RELOAD SUCCESS] Route maps and rate-limit tiers reloaded cleanly.")
			}

		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("[HOT-RELOAD ERROR] fsnotify error event: %v", err)
		}
	}
}

func (gw *Gateway) handleSecureEdgeProxy(w http.ResponseWriter, r *http.Request) {
	// 1. Resolve Route Mapping Target
	var activePool *BackendPool
	var matchedPath string

	gw.mu.RLock()
	for path, pool := range gw.routes {
		if strings.HasPrefix(r.URL.Path, path) {
			activePool = pool
			matchedPath = path
			break
		}
	}
	userTiers := gw.tiers
	gw.mu.RUnlock()

	if activePool == nil {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("404 Route Unmapped by Gateway Entry"))
		return
	}

	// 2. Extract Authorization Header Metadata
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
		log.Printf("[REJECT] Missing Bearer Layout Token on path %s", r.URL.Path)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("401 Unauthorized - Cryptographic Token Required"))
		return
	}

	tokenStr := strings.TrimPrefix(authHeader, "Bearer ")

	// 3. Cryptographically Verify Signature Identity
	token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return jwtSecret, nil
	})

	if err != nil || !token.Valid {
		log.Printf("[REJECT] Invalid Token Signature Blocked: %v", err)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte("401 Unauthorized - Bad Security Token Signature"))
		return
	}

	// 4. Extract Subscription Tier Profile Context
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	userID := fmt.Sprintf("%v", claims["sub"])
	userTier := fmt.Sprintf("%v", claims["tier"])

	limitRules, exists := userTiers[userTier]
	if !exists {
		limitRules = TierLimit{Capacity: 3, FillRate: 1} // Safe default
	}

	// 5. Evaluate Token Bucket via Atomic Redis Lua Script
	redisKey := fmt.Sprintf("ratelimit:%s:%s", userID, matchedPath)

	allowed, remaining, err := gw.limiter.Evaluate(r.Context(), redisKey, limitRules.Capacity, limitRules.FillRate)
	if err != nil {
		log.Printf("[CRASH] Redis communication pipeline failure: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("Internal Gateway Engine State Error"))
		return
	}

	// =========================================================================
	// 6. RESPONSE HEADER MANIPULATION & TELEMETRY INJECTION
	// =========================================================================
	w.Header().Set("X-RateLimit-Limit", fmt.Sprintf("%d", limitRules.Capacity))
	w.Header().Set("X-RateLimit-Remaining", fmt.Sprintf("%d", remaining))

	// If user hits the capacity wall, short-circuit the execution flow immediately
	if !allowed {
		log.Printf("[BLOCKED] User Account %s (%s tier) hit maximum capacity thresholds.", userID, userTier)
		w.Header().Set("Retry-After", "1") // Ask client to pause for 1 second before trying again
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(fmt.Sprintf("429 Too Many Requests - Your %s limit allocation is completely empty.", userTier)))
		return
	}

	// 7. Dynamic Round Robin Selection and Circuit Breaker execution
	peer := activePool.NextPeer()
	if peer == nil {
		log.Printf("[OUTAGE] All backends offline or tripped open for path: %s", matchedPath)
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("503 Service Unavailable - All backend nodes are offline or degraded"))
		return
	}

	log.Printf("[PROXY PASS] User: %s | Tier: %s | Dynamic Token Quota Left: %d | Backend: %s", userID, userTier, remaining, peer.URL.String())

	// Execute downstream request inside circuit breaker wrapper
	if err := peer.ServeHTTP(w, r); err != nil {
		if err == gobreaker.ErrOpenState {
			log.Printf("[CIRCUIT BREAKER OPEN] Request blocked to backend: %s", peer.URL.String())
			w.Header().Set("X-Circuit-State", "Open")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("503 Service Unavailable - Downstream service is currently degraded (Circuit Breaker Open)"))
			return
		}
	}
}

type statusRecorder struct {
	http.ResponseWriter
	statusCode int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.statusCode = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.statusCode == 0 {
		r.statusCode = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func observabilityMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Bypassed path
		if r.URL.Path == "/metrics" {
			next(w, r)
			return
		}

		startTime := time.Now()

		// 1. OpenTelemetry Distributed Tracing Propagation
		traceID := r.Header.Get("X-Trace-ID")
		var spanCtx trace.SpanContext

		if traceID != "" {
			if tid, err := trace.TraceIDFromHex(traceID); err == nil {
				spanCtx = trace.NewSpanContext(trace.SpanContextConfig{
					TraceID: tid,
				})
			}
		}

		ctx := r.Context()
		if spanCtx.IsValid() {
			ctx = trace.ContextWithRemoteSpanContext(ctx, spanCtx)
		}

		ctx, span := tracer.Start(ctx, fmt.Sprintf("%s %s", r.Method, r.URL.Path))
		defer span.End()

		actualTraceID := span.SpanContext().TraceID().String()

		// Inject trace ID into request headers to proxy downstream, and response headers for the client
		r.Header.Set("X-Trace-ID", actualTraceID)
		w.Header().Set("X-Trace-ID", actualTraceID)

		rec := &statusRecorder{ResponseWriter: w, statusCode: http.StatusOK}

		next(rec, r.WithContext(ctx))

		// 2. Log Metrics
		duration := time.Since(startTime).Seconds()
		statusStr := fmt.Sprintf("%d", rec.statusCode)

		// Sanitize path for metrics registry to prevent label cardinality bloating
		path := r.URL.Path
		if strings.HasPrefix(path, "/api/v1/products") {
			path = "/api/v1/products"
		} else if strings.HasPrefix(path, "/api/v1/checkout") {
			path = "/api/v1/checkout"
		}

		requestsTotal.WithLabelValues(r.Method, path, statusStr).Inc()
		requestDuration.WithLabelValues(r.Method, path, statusStr).Observe(duration)
	}
}

func runMockServer() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}
	serviceName := os.Getenv("SERVICE_NAME")
	if serviceName == "" {
		serviceName = "mock-service"
	}

	mux := http.NewServeMux()

	// Health endpoint
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{
			"status":  "UP",
			"service": serviceName,
			"time":    time.Now().Format(time.RFC3339),
		})
	})

	// Service logic with error injection features
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("fail") == "true" {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("Simulated service failure (500)"))
			return
		}

		if delayStr := r.URL.Query().Get("delay"); delayStr != "" {
			if delay, err := time.ParseDuration(delayStr); err == nil {
				time.Sleep(delay)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"message":      fmt.Sprintf("Hello from %s!", serviceName),
			"path":         r.URL.Path,
			"query":        r.URL.RawQuery,
			"headers":      r.Header,
			"service_name": serviceName,
			"port":         port,
		})
	})

	log.Printf("Mock Microservice [%s] listening on port :%s", serviceName, port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
