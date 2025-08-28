package main

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen string `yaml:"listen"`
	Routes []struct {
		Prefix string `yaml:"prefix"`
		Target string `yaml:"target"`
	} `yaml:"routes"`
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
	if err := http.ListenAndServe(listenAddr, nil); err != nil {
		log.Fatal(err)
	}
}
