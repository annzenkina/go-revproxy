# Go Reverse Proxy Load Balancer

A health-aware round-robin load balancer written in Go that distributes HTTP requests across healthy backend servers with automatic health monitoring and retry logic.

## Features

- **Health-aware load balancing** with automatic server health checks
- **Round-robin distribution** across healthy servers
- **Request retry logic** (up to 3 attempts on different servers)
- **Rate limiting** with token bucket per client IP
- **YAML configuration** with flexible routing rules
- **Health monitoring endpoint** at `/healthz`

## Quick Start

1. **Start backend servers and proxy:**
   ```bash
   make up
   ```

2. **Test the load balancer:**
   ```bash
   curl http://localhost:8080/hello
   curl http://localhost:8080/healthz
   ```

## Configuration

Copy the example config and customize as needed:
```bash
cp config.example.yaml config.yaml
```

Basic configuration:
```yaml
listen: ":8080"
routes:
  - prefix: "/server1"
    target: "http://localhost:9001"
  - prefix: "/server2"
    target: "http://localhost:9002"
healthCheckInterval: 5s
rateLimitTokenCapacity: 5
rateLimitResetInterval: 1m
```

## API Endpoints

- **`/hello`** - Load-balanced endpoint
- **`/healthz`** - Health status of all backend servers

## Development

- `make build` - Build the application
- `make run` - Run the proxy
- `make clean` - Clean build artifacts

## License

This project is open source.
