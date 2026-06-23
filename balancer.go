package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sony/gobreaker"
)

type contextKey string
const responseWrapperKey contextKey = "response_wrapper"

// Backend represents a single downstream microservice instance
type Backend struct {
	URL          *url.URL
	ReverseProxy *httputil.ReverseProxy
	Alive        bool
	CB           *gobreaker.CircuitBreaker
	mu           sync.RWMutex // Protects the Alive status from data races
}

// NewBackend initializes a Backend with its ReverseProxy and CircuitBreaker
func NewBackend(targetURL *url.URL) *Backend {
	b := &Backend{
		URL:   targetURL,
		Alive: true,
	}

	b.ReverseProxy = httputil.NewSingleHostReverseProxy(targetURL)

	// Configure a circuit breaker per backend
	b.CB = gobreaker.NewCircuitBreaker(gobreaker.Settings{
		Name:        targetURL.String(),
		MaxRequests: 3,                 // Max requests in half-open state
		Interval:    10 * time.Second,  // Interval to clear failure counts in closed state
		Timeout:     5 * time.Second,   // Time in open state before transitioning to half-open
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			// Trip breaker if failure rate exceeds 50% after at least 5 requests
			failureRatio := float64(counts.TotalFailures) / float64(counts.Requests)
			return counts.Requests >= 5 && failureRatio >= 0.5
		},
		OnStateChange: func(name string, from, to gobreaker.State) {
			log.Printf("[CIRCUIT BREAKER] State changed for '%s': %s -> %s", name, from, to)
		},
	})

	// Configure the static error handler to capture network failures thread-safely
	b.ReverseProxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("[PROXY ERROR] Downstream node %s failure: %v", targetURL.String(), err)
		if wrapperVal := r.Context().Value(responseWrapperKey); wrapperVal != nil {
			if wrapper, ok := wrapperVal.(*responseWriterWrapper); ok {
				wrapper.err = err
			}
		}
		w.WriteHeader(http.StatusBadGateway)
	}

	return b
}

// SetAlive safely mutates the health status using a Write Lock
func (b *Backend) SetAlive(alive bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.Alive = alive
}

// IsAlive safely reads the health status using a Read Lock
func (b *Backend) IsAlive() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.Alive
}

// responseWriterWrapper intercepts HTTP writes to track execution status and failures
type responseWriterWrapper struct {
	http.ResponseWriter
	statusCode int
	err        error
}

func (w *responseWriterWrapper) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *responseWriterWrapper) Write(b []byte) (int, error) {
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	if err != nil {
		w.err = err
	}
	return n, err
}

// ServeHTTP executes the backend's reverse proxy inside the Circuit Breaker
func (b *Backend) ServeHTTP(w http.ResponseWriter, r *http.Request) error {
	wrapper := &responseWriterWrapper{ResponseWriter: w, statusCode: http.StatusOK}
	ctx := context.WithValue(r.Context(), responseWrapperKey, wrapper)
	r = r.WithContext(ctx)

	_, err := b.CB.Execute(func() (interface{}, error) {
		b.ReverseProxy.ServeHTTP(wrapper, r)
		if wrapper.err != nil {
			return nil, wrapper.err
		}
		if wrapper.statusCode >= 500 {
			return nil, fmt.Errorf("backend service error: HTTP %d", wrapper.statusCode)
		}
		return nil, nil
	})

	return err
}

// BackendPool holds all our backend servers and the atomic counter
type BackendPool struct {
	backends []*Backend
	current  uint32 // Hardware-level atomic counter for routing
}

// NextPeer executes the Lock-Free Round-Robin algorithm
func (p *BackendPool) NextPeer() *Backend {
	next := int(atomic.AddUint32(&p.current, uint32(1)))
	total := len(p.backends)
	for i := 0; i < total; i++ {
		idx := (next + i) % total
		b := p.backends[idx]
		
		// Backend must be alive via health check and circuit breaker must not be open
		if b.IsAlive() && b.CB.State() != gobreaker.StateOpen {
			return b
		}
	}
	return nil
}

// HealthCheckDaemon runs in the background and pings servers
func (p *BackendPool) HealthCheckDaemon(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	// Short timeout so a dead server doesn't freeze the checker
	client := http.Client{Timeout: 2 * time.Second}

	for {
		select {
		case <-ctx.Done():
			log.Println("Health check daemon shutting down...")
			return
		case <-ticker.C:
			for _, b := range p.backends {
				// Ping the health endpoint of the backend
				resp, err := client.Get(b.URL.String() + "/health")
				
				if err != nil || resp.StatusCode != http.StatusOK {
					if b.IsAlive() {
						log.Printf("[ALERT] Node offline: %s", b.URL.String())
						b.SetAlive(false) // Safely mark as dead
					}
				} else {
					if !b.IsAlive() {
						log.Printf("[RECOVERY] Node back online: %s", b.URL.String())
						b.SetAlive(true) // Safely mark as alive
					}
				}
				
				if resp != nil {
					resp.Body.Close() // Prevent memory leaks
				}
			}
		}
	}
}