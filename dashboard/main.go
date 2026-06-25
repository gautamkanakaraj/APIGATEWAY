package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Event struct {
	Type     string `json:"type"`
	User     string `json:"user,omitempty"`
	Tier     string `json:"tier,omitempty"`
	Quota    int    `json:"quota"`
	Backend  string `json:"backend,omitempty"`
	Path     string `json:"path,omitempty"`
	Status   string `json:"status,omitempty"`
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
}

type Hub struct {
	clients    map[chan string]bool
	register   chan chan string
	unregister chan chan string
	broadcast  chan string
	mu         sync.Mutex
}

func NewHub() *Hub {
	return &Hub{
		clients:    make(map[chan string]bool),
		register:   make(chan chan string),
		unregister: make(chan chan string),
		broadcast:  make(chan string),
	}
}

func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client] = true
			h.mu.Unlock()
		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client)
			}
			h.mu.Unlock()
		case message := <-h.broadcast:
			h.mu.Lock()
			for client := range h.clients {
				select {
				case client <- message:
				default:
					close(client)
					delete(h.clients, client)
				}
			}
			h.mu.Unlock()
		}
	}
}

func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported!", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	clientChan := make(chan string, 20)
	h.register <- clientChan

	defer func() {
		h.unregister <- clientChan
	}()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-clientChan:
			if !ok {
				return
			}
			_, err := fmt.Fprintf(w, "data: %s\n\n", msg)
			if err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func main() {
	hub := NewHub()
	go hub.Run()

	// 1. Start Docker Log Scraper in the background
	go tailGatewayLogs(hub)

	// 2. Start Service Health Checker
	go runHealthChecks(hub)

	// 3. Serve Frontend Assets
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, "index.html")
	})

	http.Handle("/stream", hub)

	log.Println("Dashboard Backend listening on port :3000...")
	log.Fatal(http.ListenAndServe(":3000", nil))
}

func runHealthChecks(hub *Hub) {
	backends := []string{
		"http://service-products-1:8081",
		"http://service-products-2:8083",
		"http://service-checkout-1:8082",
	}

	client := http.Client{Timeout: 1 * time.Second}

	for {
		for _, b := range backends {
			resp, err := client.Get(b + "/health")
			status := "OFFLINE"
			if err == nil && resp.StatusCode == http.StatusOK {
				status = "ONLINE"
			}
			if resp != nil {
				resp.Body.Close()
			}

			evt := Event{
				Type:    "health",
				Backend: b,
				Status:  status,
			}
			data, _ := json.Marshal(evt)
			hub.broadcast <- string(data)
		}
		time.Sleep(3 * time.Second)
	}
}

func tailGatewayLogs(hub *Hub) {
	proxyRegex := regexp.MustCompile(`\[PROXY PASS\] User:\s*(\S+)\s*\|\s*Tier:\s*(\S+)\s*\|\s*Dynamic Token Quota Left:\s*(\d+)\s*\|\s*Backend:\s*(\S+)`)
	blockedRegex := regexp.MustCompile(`\[BLOCKED\] User Account\s*(\S+)\s*\((\S+)\s*tier\)\s*hit maximum capacity thresholds`)
	alertRegex := regexp.MustCompile(`\[ALERT\] Node offline:\s*(\S+)`)
	recoveryRegex := regexp.MustCompile(`\[RECOVERY\] Node back online:\s*(\S+)`)
	cbRegex := regexp.MustCompile(`\[CIRCUIT BREAKER\] State changed for\s*'([^']+)':\s*(\S+)\s*->\s*(\S+)`)

	client := &http.Client{
		Transport: &http.Transport{
			Dial: func(network, addr string) (net.Conn, error) {
				return net.Dial("unix", "/var/run/docker.sock")
			},
		},
	}

	for {
		log.Println("Connecting to Docker socket to stream api-gateway logs...")
		resp, err := client.Get("http://localhost/containers/api-gateway/logs?stdout=true&stderr=true&follow=true&tail=50")
		if err != nil {
			log.Printf("Failed to connect to Docker logs API: %v. Retrying in 2s...", err)
			time.Sleep(2 * time.Second)
			continue
		}

		log.Println("Successfully attached to api-gateway logs stream.")

		reader := bufio.NewReader(resp.Body)
		for {
			// Read Docker multiplexed stream header
			header := make([]byte, 8)
			_, err := io.ReadFull(reader, header)
			if err != nil {
				log.Printf("Log stream connection closed: %v. Reconnecting...", err)
				break
			}

			size := binary.BigEndian.Uint32(header[4:8])
			payload := make([]byte, size)
			_, err = io.ReadFull(reader, payload)
			if err != nil {
				log.Printf("Log stream payload read error: %v. Reconnecting...", err)
				break
			}

			logLine := string(payload)
			lines := strings.Split(logLine, "\n")
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}

				var evt *Event

				// 1. PROXY PASS
				if matches := proxyRegex.FindStringSubmatch(line); matches != nil {
					quota, _ := strconv.Atoi(matches[3])
					evt = &Event{
						Type:    "proxy_pass",
						User:    matches[1],
						Tier:    matches[2],
						Quota:   quota,
						Backend: matches[4],
					}
				}

				// 2. BLOCKED (429)
				if matches := blockedRegex.FindStringSubmatch(line); matches != nil {
					evt = &Event{
						Type:   "blocked",
						User:   matches[1],
						Tier:   matches[2],
						Path:   "/api/v1/products",
						Status: "429",
					}
				}

				// 3. HEALTH ALERT
				if matches := alertRegex.FindStringSubmatch(line); matches != nil {
					evt = &Event{
						Type:    "health",
						Backend: matches[1],
						Status:  "OFFLINE",
					}
				}

				// 4. HEALTH RECOVERY
				if matches := recoveryRegex.FindStringSubmatch(line); matches != nil {
					evt = &Event{
						Type:    "health",
						Backend: matches[1],
						Status:  "ONLINE",
					}
				}

				// 5. CIRCUIT BREAKER
				if matches := cbRegex.FindStringSubmatch(line); matches != nil {
					evt = &Event{
						Type:    "circuit_breaker",
						Backend: matches[1],
						From:    matches[2],
						To:      matches[3],
					}
				}

				if evt != nil {
					data, err := json.Marshal(evt)
					if err == nil {
						hub.broadcast <- string(data)
					}
				}
			}
		}
		resp.Body.Close()
		time.Sleep(2 * time.Second)
	}
}
