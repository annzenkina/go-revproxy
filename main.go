package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"time"

	"gopkg.in/yaml.v3"
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

func checkServerHealth(serverURL string) ServerStatus {
	client := &http.Client{
		Timeout: 5 * time.Second,
	}

	healthURL := serverURL + "/healthz"
	resp, err := client.Get(healthURL)

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

func getNextServer(counter int16, urls []*url.URL) *url.URL {
	index := int(counter) % len(urls)

	switch index {
	case 0:
		return urls[0] // Return first URL
	case 1:
		return urls[1] // Return second URL
	case 2:
		return urls[2] // Return third URL
	default:
		return urls[0] // Default to first URL
	}
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

	counter := int16(0)
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

	// Add health check endpoint
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		var serverStatuses []ServerStatus
		for _, route := range config.Routes {
			status := checkServerHealth(route.Target)
			serverStatuses = append(serverStatuses, status)
		}

		// Return the server statuses directly
		json.NewEncoder(w).Encode(serverStatuses)
	})

	// Create handlers for each route
	for _, route := range config.Routes {
		prefix := route.Prefix
		http.HandleFunc(prefix+"/", func(w http.ResponseWriter, r *http.Request) {
			counter++
			nextURL := getNextServer(counter, targetURLs)
			proxy := httputil.NewSingleHostReverseProxy(nextURL)
			log.Printf("Request %d: forwarding to %s", counter, nextURL.String())
			proxy.ServeHTTP(w, r)
		})
	}

	log.Printf("listening on %s, forwarding based on config", listenAddr)
	log.Printf("Health checks available at /healthz")
	if err := http.ListenAndServe(listenAddr, nil); err != nil {
		log.Fatal(err)
	}
}
