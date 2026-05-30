

package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

		"gateway/limiter"   //limiter file
	"github.com/golang-jwt/jwt/v5"
	"gopkg.in/yaml.v3"
)

var jwtSecret = []byte("super-secret-gateway-key")

type TierLimit struct {
	Capacity int `yaml:"capacity"`
	FillRate int `yaml:"fill_rate"`
}

type Config struct {
	Server struct {
		Port string `yaml:"port"`
	} `yaml:"server"`
	Tiers  map[string]TierLimit `yaml:"tiers"`
	Routes []RouteConfig        `yaml:"routes"`
}

type RouteConfig struct {
	Path   string `yaml:"path"`
	Target string `yaml:"target"`
}

type Gateway struct {
	routes  map[string]*httputil.ReverseProxy
	tiers   map[string]TierLimit
	limiter *limiter.RateLimiter
}

func main() {
	// 1. Parse Gateway YAML Configuration
	yamlFile, err := ioutil.ReadFile("gateway.yaml")
	if err != nil {
		log.Fatalf("Config read error: %v", err)
	}

	var config Config
	if err := yaml.Unmarshal(yamlFile, &config); err != nil {
		log.Fatalf("Config parse error: %v", err)
	}

	// 2. Initialize Redis Connection Pool on standard port 6379
	rl := limiter.NewRateLimiter("localhost:6379")

	gw := &Gateway{
		routes:  make(map[string]*httputil.ReverseProxy),
		tiers:   config.Tiers,
		limiter: rl,
	}

	// 3. Mount HTTP Reverse Proxy Targets
	for _, r := range config.Routes {
		targetURL, err := url.Parse(r.Target)
		if err != nil {
			log.Fatalf("Invalid target URL setup %s: %v", r.Target, err)
		}
		gw.routes[r.Path] = httputil.NewSingleHostReverseProxy(targetURL)
		fmt.Printf("Shield Armed: Route Prefix %s ➡️ %s\n", r.Path, r.Target)
	}

	http.HandleFunc("/", gw.handleSecureEdgeProxy)

	fmt.Printf("Telemetry-Enabled API Gateway listening on port :%s\n", config.Server.Port)
	log.Fatal(http.ListenAndServe(":"+config.Server.Port, nil))
}

func (gw *Gateway) handleSecureEdgeProxy(w http.ResponseWriter, r *http.Request) {
	// 1. Resolve Route Mapping Target
	var activeProxy *httputil.ReverseProxy
	var matchedPath string
	for path, proxy := range gw.routes {
		if strings.HasPrefix(r.URL.Path, path) {
			activeProxy = proxy
			matchedPath = path
			break
		}
	}

	if activeProxy == nil {
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

	limitRules, exists := gw.tiers[userTier]
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

	// 7. SUCCESSFUL PROXY PASS-THROUGH HANDOFF
	log.Printf("[PROXY PASS] User: %s | Tier: %s | Dynamic Token Quota Left: %d", userID, userTier, remaining)
	activeProxy.ServeHTTP(w, r)
}
