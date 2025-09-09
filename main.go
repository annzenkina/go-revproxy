package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	HealthCheckInterval = 5 * time.Second
)

type Config struct {
	Listen string `yaml:"listen"`
	Routes []struct {
		Prefix string `yaml:"prefix"`
		Target string `yaml:"target"`
	} `yaml:"routes"`
}

type ServerStatus struct {
	URL     string `json:"url"`
	Healthy bool   `json:"healthy"`
	Error   string `json:"error,omitempty"`
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

	// Start periodic health checks
	loadBalancer.StartHealthChecks(HealthCheckInterval)

	// Add health check endpoint
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		serverStatuses := loadBalancer.GetServerStatuses()
		json.NewEncoder(w).Encode(serverStatuses)
	})

	// Add load-balanced /hello endpoint
	http.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
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
	log.Printf("Starting health checks every %v", HealthCheckInterval)
	if err := http.ListenAndServe(listenAddr, nil); err != nil {
		log.Fatal(err)
	}
}
