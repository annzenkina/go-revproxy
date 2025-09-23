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
	// Default health check configuration
	DefaultHealthCheckMaxRetries = 1
	DefaultHealthCheckRetryDelay = 1 * time.Second
	DefaultHealthCheckTimeout    = 5 * time.Second
	// Exponential backoff multiplier
	ExponentialBackoffMultiplier = 1.5
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
	// Health check failure configuration
	HealthCheckFailureCodes []int         `yaml:"healthCheckFailureCodes"` // HTTP status codes that indicate failure
	HealthCheckMaxRetries   int           `yaml:"healthCheckMaxRetries"`   // Max retries before removing server from rotation
	HealthCheckRetryDelay   time.Duration `yaml:"healthCheckRetryDelay"`   // Initial delay between retries (exponential backoff)
	HealthCheckTimeout      time.Duration `yaml:"healthCheckTimeout"`      // Timeout for individual health check requests
}

type ServerStatus struct {
	URL     string `json:"url"`
	Healthy bool   `json:"healthy"`
	Error   string `json:"error,omitempty"`
}

// ServerState tracks detailed state information for each server
type ServerState struct {
	URL           string
	Healthy       bool
	InRotation    bool          // Whether server is currently in rotation
	RetryCount    int           // Current retry attempt count
	LastHealthy   time.Time     // Last time server was healthy
	LastFailed    time.Time     // Last time server failed
	NextRetryTime time.Time     // When to attempt next retry
	RetryDelay    time.Duration // Current retry delay (for exponential backoff)
	LastError     string
	mu            sync.RWMutex // Mutex for thread-safe access
}

// NewServerState creates a new ServerState with default values
func NewServerState(url string, initialDelay time.Duration) *ServerState {
	return &ServerState{
		URL:         url,
		Healthy:     true,
		InRotation:  true,
		RetryCount:  0,
		LastHealthy: time.Now(),
		RetryDelay:  initialDelay,
	}
}

// calculateExponentialBackoff calculates the next retry delay and time
func (ss *ServerState) calculateExponentialBackoff(multiplier float64) {
	nextDelay := time.Duration(float64(ss.RetryDelay) * multiplier)
	ss.NextRetryTime = time.Now().Add(ss.RetryDelay) // Use current delay for this retry
	ss.RetryDelay = nextDelay
}

// MarkHealthy marks the server as healthy and resets retry state
func (ss *ServerState) MarkHealthy(initialDelay time.Duration) {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	wasUnhealthy := !ss.Healthy
	ss.Healthy = true
	ss.InRotation = true
	ss.RetryCount = 0
	ss.RetryDelay = initialDelay
	ss.LastHealthy = time.Now()
	ss.LastError = ""

	if wasUnhealthy {
		log.Printf("Server %s recovered and is now healthy", ss.URL)
	}
}

// MarkUnhealthy marks the server as unhealthy and updates retry state
func (ss *ServerState) MarkUnhealthy(errorMsg string, maxRetries int) {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	ss.Healthy = false
	ss.LastFailed = time.Now()
	ss.LastError = errorMsg
	ss.RetryCount++

	// Calculate next retry time with exponential backoff
	ss.calculateExponentialBackoff(ExponentialBackoffMultiplier)

	// Remove from rotation if max retries exceeded
	if ss.RetryCount >= maxRetries {
		ss.InRotation = false
		log.Printf("Server %s removed from rotation after %d failed attempts", ss.URL, ss.RetryCount)
	} else {
		log.Printf("Server %s marked unhealthy (attempt %d/%d), next retry at %v: %s",
			ss.URL, ss.RetryCount, maxRetries, ss.NextRetryTime.Format(time.RFC3339), errorMsg)
	}
}

// ShouldRetry checks if it's time to retry this server
func (ss *ServerState) ShouldRetry(maxRetries int) bool {
	ss.mu.RLock()
	defer ss.mu.RUnlock()

	return !ss.Healthy && ss.RetryCount < maxRetries && time.Now().After(ss.NextRetryTime)
}

// IsInRotation checks if the server is currently in rotation
func (ss *ServerState) IsInRotation() bool {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	return ss.InRotation
}

// GetStatus returns a ServerStatus for API compatibility
func (ss *ServerState) GetStatus() ServerStatus {
	ss.mu.RLock()
	defer ss.mu.RUnlock()

	return ServerStatus{
		URL:     ss.URL,
		Healthy: ss.Healthy,
		Error:   ss.LastError,
	}
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
	statuses      []ServerStatus // For backward compatibility with health endpoint
	serverStates  []*ServerState // Enhanced state tracking
	counter       int32
	mu            sync.RWMutex
	operational   bool
	operationalMu sync.RWMutex
	config        *Config // Reference to configuration
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

			serverState := lb.serverStates[index]

			// Get config values
			timeout := DefaultHealthCheckTimeout
			maxRetries := DefaultHealthCheckMaxRetries
			initialDelay := DefaultHealthCheckRetryDelay
			var failureCodes []int
			if lb.config != nil {
				if lb.config.HealthCheckTimeout > 0 {
					timeout = lb.config.HealthCheckTimeout
				}
				if lb.config.HealthCheckMaxRetries > 0 {
					maxRetries = lb.config.HealthCheckMaxRetries
				}
				if lb.config.HealthCheckRetryDelay > 0 {
					initialDelay = lb.config.HealthCheckRetryDelay
				}
				failureCodes = lb.config.HealthCheckFailureCodes
			}

			// Check if this server should be retried (for failed servers)
			if !serverState.IsInRotation() || serverState.ShouldRetry(maxRetries) {
				status := checkServerHealthWithConfig(serverURL.String(), failureCodes, timeout)

				if status.Healthy {
					serverState.MarkHealthy(initialDelay)
				} else {
					serverState.MarkUnhealthy(status.Error, maxRetries)
				}
			}

			// Update backward-compatible status
			lb.mu.Lock()
			lb.statuses[index] = serverState.GetStatus()
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

	// Get servers that are healthy and in rotation
	var healthyServers []*url.URL
	for i, serverState := range lb.serverStates {
		if serverState.IsInRotation() && serverState.Healthy {
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

// MarkServerFailed marks a server as failed during request processing
func (lb *LoadBalancer) MarkServerFailed(serverURL string, errorMsg string) {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	// Get max retries from config
	maxRetries := DefaultHealthCheckMaxRetries
	if lb.config != nil && lb.config.HealthCheckMaxRetries > 0 {
		maxRetries = lb.config.HealthCheckMaxRetries
	}

	// Find and update the server state
	for i, server := range lb.servers {
		if server.String() == serverURL {
			lb.serverStates[i].MarkUnhealthy(errorMsg, maxRetries)
			lb.statuses[i] = lb.serverStates[i].GetStatus()
			break
		}
	}

	// Update operational status
	go lb.updateOperationalStatus()
}

// TriggerImmediateHealthCheck performs an immediate health check on a specific server
func (lb *LoadBalancer) TriggerImmediateHealthCheck(serverURL string) {
	go func() {
		// Get config values
		timeout := DefaultHealthCheckTimeout
		maxRetries := DefaultHealthCheckMaxRetries
		initialDelay := DefaultHealthCheckRetryDelay
		var failureCodes []int
		if lb.config != nil {
			if lb.config.HealthCheckTimeout > 0 {
				timeout = lb.config.HealthCheckTimeout
			}
			if lb.config.HealthCheckMaxRetries > 0 {
				maxRetries = lb.config.HealthCheckMaxRetries
			}
			if lb.config.HealthCheckRetryDelay > 0 {
				initialDelay = lb.config.HealthCheckRetryDelay
			}
			failureCodes = lb.config.HealthCheckFailureCodes
		}

		status := checkServerHealthWithConfig(serverURL, failureCodes, timeout)

		lb.mu.Lock()
		// Find and update the status of this specific server
		for i, server := range lb.servers {
			if server.String() == serverURL {
				if status.Healthy {
					lb.serverStates[i].MarkHealthy(initialDelay)
				} else {
					lb.serverStates[i].MarkUnhealthy(status.Error, maxRetries)
				}
				lb.statuses[i] = lb.serverStates[i].GetStatus()
				break
			}
		}
		lb.mu.Unlock()
		lb.updateOperationalStatus()
	}()
}

func checkServerHealthWithConfig(serverURL string, failureCodes []int, timeout time.Duration) ServerStatus {
	// Create HTTP client with timeout
	client := &http.Client{
		Timeout: timeout,
	}

	resp, err := client.Get(serverURL + "/")
	if err != nil {
		return ServerStatus{
			URL:     serverURL,
			Healthy: false,
			Error:   err.Error(),
		}
	}
	defer resp.Body.Close()

	// Check if status code indicates failure
	if isFailureStatusCode(resp.StatusCode, failureCodes) {
		return ServerStatus{
			URL:     serverURL,
			Healthy: false,
			Error:   fmt.Sprintf("HTTP %d", resp.StatusCode),
		}
	}

	// Try to parse the server's response as JSON
	var serverResponse ServerStatus
	if err := json.NewDecoder(resp.Body).Decode(&serverResponse); err != nil {
		// Standard approach: any HTTP 200-299 response is considered healthy
		// This is how most load balancers and health check systems work
		log.Printf("Info: Server %s responded with HTTP %d (non-JSON), treating as healthy", serverURL, resp.StatusCode)
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

// isFailureStatusCode checks if a status code indicates failure
func isFailureStatusCode(statusCode int, failureCodes []int) bool {
	// If no failure codes are configured, use default behavior
	if len(failureCodes) == 0 {
		// Default: anything outside 200-299 range is considered failure
		return statusCode < 200 || statusCode >= 300
	}

	// Check against configured failure codes
	for _, code := range failureCodes {
		if code == statusCode {
			return true
		}
	}

	return false
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

	// Set default health check configuration if not provided
	if len(config.HealthCheckFailureCodes) == 0 {
		config.HealthCheckFailureCodes = []int{502} // Default failure code as requested
	}
	if config.HealthCheckMaxRetries <= 0 {
		config.HealthCheckMaxRetries = DefaultHealthCheckMaxRetries
	}
	if config.HealthCheckRetryDelay <= 0 {
		config.HealthCheckRetryDelay = DefaultHealthCheckRetryDelay
	}
	if config.HealthCheckTimeout <= 0 {
		config.HealthCheckTimeout = DefaultHealthCheckTimeout
	}

	// Create server states
	serverStates := make([]*ServerState, len(targetURLs))
	for i, server := range targetURLs {
		serverStates[i] = NewServerState(
			server.String(),
			config.HealthCheckRetryDelay,
		)
	}

	// Create load balancer
	loadBalancer := &LoadBalancer{
		servers:      targetURLs,
		statuses:     make([]ServerStatus, len(targetURLs)),
		serverStates: serverStates,
		operational:  true, // Assume operational initially
		config:       config,
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

	// Initialize statuses from server states
	for i, serverState := range serverStates {
		loadBalancer.statuses[i] = serverState.GetStatus()
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

				// Mark server as failed for immediate removal from rotation
				loadBalancer.MarkServerFailed(nextURL.String(), err.Error())

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
	log.Printf("Health check configuration: failure codes %v, max retries %d, retry delay %v, timeout %v",
		config.HealthCheckFailureCodes, config.HealthCheckMaxRetries, config.HealthCheckRetryDelay, config.HealthCheckTimeout)

	// Ensure rate limiter cleanup routine is stopped on exit
	defer rateLimiter.Stop()

	if err := http.ListenAndServe(listenAddr, nil); err != nil {
		log.Fatal(err)
	}
}
