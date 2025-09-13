package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	HealthCheckInterval        = 5 * time.Second
	DefaultTokenBucketCapacity = 5
	DefaultTokenResetInterval  = 1 * time.Minute
)

type Config struct {
	Listen string `yaml:"listen"`
	Routes []struct {
		Prefix string `yaml:"prefix"`
		Target string `yaml:"target"`
	} `yaml:"routes"`
	RateLimitTokenCapacity int           `yaml:"rateLimitTokenCapacity"`
	RateLimitResetInterval time.Duration `yaml:"rateLimitResetInterval"`
	HealthCheckInterval    time.Duration `yaml:"healthCheckInterval"`
}

type ServerStatus struct {
	URL     string `json:"url"`
	Healthy bool   `json:"healthy"`
	Error   string `json:"error,omitempty"`
}

// TokenBucket represents a token bucket for rate limiting
type TokenBucket struct {
	capacity      int           // Maximum number of tokens in bucket
	tokens        int           // Current number of tokens
	lastReset     time.Time     // Last time tokens were reset to full capacity
	resetInterval time.Duration // Interval at which tokens reset to full capacity
	mu            sync.Mutex    // Mutex for thread safety
}

// NewTokenBucket creates a new token bucket
func NewTokenBucket(capacity int, resetInterval time.Duration) *TokenBucket {
	return &TokenBucket{
		capacity:      capacity,
		tokens:        capacity, // Start with full capacity
		lastReset:     time.Now(),
		resetInterval: resetInterval,
	}
}

// Allow checks if a request should be allowed (consumes a token)
func (tb *TokenBucket) Allow() bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	now := time.Now()

	// Check if it's time to reset tokens to full capacity
	if now.Sub(tb.lastReset) >= tb.resetInterval {
		tb.tokens = tb.capacity
		tb.lastReset = now
	}

	// Check if we have tokens available
	if tb.tokens > 0 {
		tb.tokens--
		return true
	}

	return false
}

// RateLimiter manages rate limiting for multiple clients
type RateLimiter struct {
	buckets       map[string]*TokenBucket
	mu            sync.RWMutex
	cleanup       chan struct{}
	capacity      int
	resetInterval time.Duration
}

// NewRateLimiter creates a new rate limiter
func NewRateLimiter(capacity int, resetInterval time.Duration) *RateLimiter {
	rl := &RateLimiter{
		buckets:       make(map[string]*TokenBucket),
		cleanup:       make(chan struct{}),
		capacity:      capacity,
		resetInterval: resetInterval,
	}

	// Start cleanup goroutine to remove old buckets
	go rl.cleanupRoutine()

	return rl
}

// Allow checks if a request from the given client should be allowed
func (rl *RateLimiter) Allow(clientIP string) bool {
	rl.mu.Lock()
	bucket, exists := rl.buckets[clientIP]
	if !exists {
		bucket = NewTokenBucket(rl.capacity, rl.resetInterval)
		rl.buckets[clientIP] = bucket
	}
	rl.mu.Unlock()

	return bucket.Allow()
}

// cleanupRoutine periodically removes unused buckets to prevent memory leaks
func (rl *RateLimiter) cleanupRoutine() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			rl.mu.Lock()
			now := time.Now()
			for ip, bucket := range rl.buckets {
				// Remove buckets that haven't been used for 10 minutes
				if now.Sub(bucket.lastReset) > 10*time.Minute {
					delete(rl.buckets, ip)
				}
			}
			rl.mu.Unlock()
		case <-rl.cleanup:
			return
		}
	}
}

// Stop stops the cleanup routine
func (rl *RateLimiter) Stop() {
	close(rl.cleanup)
}

// getClientIP extracts the client IP address from the request
func getClientIP(r *http.Request) string {
	// Check X-Forwarded-For header first (for proxied requests)
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// X-Forwarded-For can contain multiple IPs, take the first one
		if idx := strings.Index(xff, ","); idx != -1 {
			return strings.TrimSpace(xff[:idx])
		}
		return strings.TrimSpace(xff)
	}

	// Check X-Real-IP header
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
	}

	// Fall back to RemoteAddr
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr // Return as-is if parsing fails
	}
	return ip
}

type LoadBalancer struct {
	servers       []*url.URL
	statuses      []ServerStatus
	counter       int32
	mu            sync.RWMutex
	operational   bool
	operationalMu sync.RWMutex
}

func (lb *LoadBalancer) StartHealthChecks(interval time.Duration) {
	ticker := time.NewTicker(interval)
	go func() {
		for range ticker.C {
			lb.performHealthChecks()
		}
	}()
}

func (lb *LoadBalancer) performHealthChecks() {
	var wg sync.WaitGroup
	wg.Add(len(lb.servers))

	for i, server := range lb.servers {
		go func(index int, serverURL *url.URL) {
			defer wg.Done()
			status := checkServerHealth(serverURL.String())
			lb.mu.Lock()
			lb.statuses[index] = status
			lb.mu.Unlock()
		}(i, server)
	}

	wg.Wait()

	// Update operational status directly after health checks complete
	lb.updateOperationalStatus()
}

func (lb *LoadBalancer) updateOperationalStatus() {
	lb.mu.RLock()
	healthyCount := 0
	for _, status := range lb.statuses {
		if status.Healthy {
			healthyCount++
		}
	}
	lb.mu.RUnlock()

	lb.operationalMu.Lock()
	wasOperational := lb.operational
	lb.operational = healthyCount > 0
	lb.operationalMu.Unlock()

	if !lb.operational && wasOperational {
		log.Printf("CRITICAL: All servers are down! Load balancer entering failure mode.")
	} else if lb.operational && !wasOperational {
		log.Printf("RECOVERY: Servers are back online! Load balancer operational again.")
	}
}

func (lb *LoadBalancer) GetNextHealthyServer() *url.URL {
	lb.mu.RLock()
	defer lb.mu.RUnlock()

	// Get healthy servers
	var healthyServers []*url.URL
	for i, status := range lb.statuses {
		if status.Healthy {
			healthyServers = append(healthyServers, lb.servers[i])
		}
	}

	if len(healthyServers) == 0 {
		return nil // No healthy servers available
	}

	// Round-robin among healthy servers
	counter := atomic.AddInt32(&lb.counter, 1)
	index := int(counter) % len(healthyServers)
	return healthyServers[index]
}

func (lb *LoadBalancer) GetServerStatuses() []ServerStatus {
	lb.mu.RLock()
	defer lb.mu.RUnlock()

	statuses := make([]ServerStatus, len(lb.statuses))
	copy(statuses, lb.statuses)
	return statuses
}

func (lb *LoadBalancer) IsOperational() bool {
	lb.operationalMu.RLock()
	defer lb.operationalMu.RUnlock()
	return lb.operational
}

// TriggerImmediateHealthCheck performs an immediate health check on a specific server
func (lb *LoadBalancer) TriggerImmediateHealthCheck(serverURL string) {
	go func() {
		status := checkServerHealthWithRetry(serverURL)
		lb.mu.Lock()
		// Find and update the status of this specific server
		for i, server := range lb.servers {
			if server.String() == serverURL {
				oldStatus := lb.statuses[i].Healthy
				lb.statuses[i] = status
				if oldStatus != status.Healthy {
					if status.Healthy {
						log.Printf("Server %s recovered and is now healthy", serverURL)
					} else {
						log.Printf("Server %s marked as unhealthy: %s", serverURL, status.Error)
					}
				}
				break
			}
		}
		lb.mu.Unlock()
		lb.updateOperationalStatus()
	}()
}

func checkServerHealth(serverURL string) ServerStatus {
	resp, err := http.Get(serverURL + "/")

	if err != nil {
		return ServerStatus{
			URL:     serverURL,
			Healthy: false,
			Error:   err.Error(),
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ServerStatus{
			URL:     serverURL,
			Healthy: false,
			Error:   fmt.Sprintf("HTTP %d", resp.StatusCode),
		}
	}

	// Try to parse the server's response as JSON
	var serverResponse ServerStatus
	if err := json.NewDecoder(resp.Body).Decode(&serverResponse); err != nil {
		// Standard approach: any HTTP 200 response is considered healthy
		// This is how most load balancers and health check systems work
		log.Printf("Info: Server %s responded with HTTP 200 (non-JSON), treating as healthy", serverURL)
		return ServerStatus{
			URL:     serverURL,
			Healthy: true,
			Error:   "",
		}
	}

	// Use the parsed response, but ensure the URL is set correctly
	serverResponse.URL = serverURL
	return serverResponse
}

// checkServerHealthWithRetry performs health check with retry logic
func checkServerHealthWithRetry(serverURL string) ServerStatus {
	maxRetries := 3
	var lastStatus ServerStatus

	for attempt := 1; attempt <= maxRetries; attempt++ {
		log.Printf("Health check attempt %d/%d for %s", attempt, maxRetries, serverURL)
		status := checkServerHealth(serverURL)

		if status.Healthy {
			if attempt > 1 {
				log.Printf("Server %s recovered after %d attempts", serverURL, attempt)
			}
			return status
		}

		lastStatus = status
		log.Printf("Health check failed for %s (attempt %d/%d): %s", serverURL, attempt, maxRetries, status.Error)

		// Don't wait after the last attempt
		if attempt < maxRetries {
			time.Sleep(time.Duration(attempt) * time.Second) // Progressive backoff: 1s, 2s, 3s
		}
	}

	log.Printf("Server %s failed all %d health check attempts, marking as unhealthy", serverURL, maxRetries)
	return lastStatus
}

// loadConfig reads and parses the YAML configuration file
func loadConfig(filename string) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}

	var config Config
	err = yaml.Unmarshal(data, &config)
	if err != nil {
		return nil, err
	}

	return &config, nil
}

func main() {
	// Load configuration from YAML file
	config, err := loadConfig("config.yaml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	listenAddr := config.Listen

	// Parse all target URLs from config
	targetURLs := make([]*url.URL, len(config.Routes))
	for i, route := range config.Routes {
		targetURL, err := url.Parse(route.Target)
		if err != nil {
			log.Fatalf("bad target %s: %v", route.Target, err)
		}
		targetURLs[i] = targetURL
	}

	// Create load balancer
	loadBalancer := &LoadBalancer{
		servers:     targetURLs,
		statuses:    make([]ServerStatus, len(targetURLs)),
		operational: true, // Assume operational initially
	}

	// Create rate limiter using configuration with sensible defaults
	tokenCapacity := config.RateLimitTokenCapacity
	if tokenCapacity <= 0 {
		tokenCapacity = DefaultTokenBucketCapacity
	}
	resetInterval := config.RateLimitResetInterval
	if resetInterval <= 0 {
		resetInterval = DefaultTokenResetInterval
	}
	rateLimiter := NewRateLimiter(tokenCapacity, resetInterval)

	// Initialize statuses
	for i, server := range targetURLs {
		loadBalancer.statuses[i] = ServerStatus{
			URL:     server.String(),
			Healthy: true, // Assume healthy initially
		}
	}

	// Run initial health check immediately
	log.Printf("Running initial health check...")
	loadBalancer.performHealthChecks()

	// Start periodic health checks (configurable)
	hcInterval := config.HealthCheckInterval
	if hcInterval <= 0 {
		hcInterval = HealthCheckInterval
	}
	loadBalancer.StartHealthChecks(hcInterval)

	// Add health check endpoint
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		// Rate limiting check
		clientIP := getClientIP(r)
		if !rateLimiter.Allow(clientIP) {
			log.Printf("Rate limit exceeded for client %s on healthz endpoint", clientIP)
			http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		serverStatuses := loadBalancer.GetServerStatuses()
		json.NewEncoder(w).Encode(serverStatuses)
	})

	// Add load-balanced /hello endpoint
	http.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		// Rate limiting check
		clientIP := getClientIP(r)
		if !rateLimiter.Allow(clientIP) {
			log.Printf("Rate limit exceeded for client %s", clientIP)
			http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
			return
		}

		// Check if load balancer is operational
		if !loadBalancer.IsOperational() {
			log.Printf("Load balancer not operational - all servers down for request to %s", r.URL.Path)
			http.Error(w, "Bad Gateway - All backend servers are down", http.StatusBadGateway)
			return
		}

		// Simple retry logic: try up to 3 different healthy servers
		maxRetries := 3

		for attempt := 0; attempt < maxRetries; attempt++ {
			nextURL := loadBalancer.GetNextHealthyServer()
			if nextURL == nil {
				log.Printf("No healthy servers available for request to %s", r.URL.Path)
				http.Error(w, "Bad Gateway - No healthy backend servers", http.StatusBadGateway)
				return
			}

			// Create a simple reverse proxy
			proxy := httputil.NewSingleHostReverseProxy(nextURL)
			requestFailed := false

			// Custom error handler to catch connection failures
			proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, err error) {
				log.Printf("Request failed to %s: %v", nextURL.String(), err)
				requestFailed = true

				// Trigger immediate health check for the failed server
				log.Printf("Triggering immediate health check for failed server: %s", nextURL.String())
				loadBalancer.TriggerImmediateHealthCheck(nextURL.String())

				// Only send error response on the last attempt
				if attempt == maxRetries-1 {
					log.Printf("All retry attempts exhausted for %s", r.URL.Path)
					http.Error(rw, "Bad Gateway - All backend servers failed", http.StatusBadGateway)
				}
			}

			log.Printf("Request: forwarding %s to %s", r.URL.Path, nextURL.String())

			// Try to serve the request
			proxy.ServeHTTP(w, r)

			// If request succeeded, we're done
			if !requestFailed {
				return
			}

			// If this was the last attempt, error response already sent
			if attempt == maxRetries-1 {
				return
			}

			// Brief pause before retrying to allow immediate health check to complete
			time.Sleep(100 * time.Millisecond)
		}
	})

	log.Printf("listening on %s, forwarding based on config", listenAddr)
	log.Printf("Health checks available at /healthz")
	log.Printf("Starting health checks every %v", hcInterval)
	log.Printf("Rate limiting enabled: Token bucket with capacity %d tokens, resets to full capacity every %v", tokenCapacity, resetInterval)

	// Ensure rate limiter cleanup routine is stopped on exit
	defer rateLimiter.Stop()

	if err := http.ListenAndServe(listenAddr, nil); err != nil {
		log.Fatal(err)
	}
}
