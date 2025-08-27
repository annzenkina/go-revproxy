package main

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
)

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

func main() {
	counter := int16(0)
	listenAddr := ":8080"
	targets := []string{
		"http://localhost:9001",
		"http://localhost:9002",
		"http://localhost:9003",
	}

	// Parse all target URLs
	targetURLs := make([]*url.URL, len(targets))
	for i, targetStr := range targets {
		targetURL, err := url.Parse(targetStr)
		if err != nil {
			log.Fatalf("bad target %s: %v", targetStr, err)
		}
		targetURLs[i] = targetURL
	}

	// Create a handler that increments counter for each request
	http.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		counter++
		nextURL := getNextServer(counter, targetURLs)
		proxy := httputil.NewSingleHostReverseProxy(nextURL)
		log.Printf("Request %d: forwarding to %s", counter, nextURL.String())
		proxy.ServeHTTP(w, r)
	})

	log.Printf("listening on :8080, forwarding -> :900X")
	if err := http.ListenAndServe(listenAddr, nil); err != nil {
		log.Fatal(err)
	}
}
