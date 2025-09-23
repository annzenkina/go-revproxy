# Go Reverse Proxy Load Balancer

A health-aware round-robin load balancer written in Go that intelligently distributes incoming HTTP requests across healthy backend servers with automatic health monitoring and graceful failure handling.

## Features

- **Health-aware load balancing**: Automatically skips unhealthy servers and distributes requests only to healthy ones
- **Round-robin distribution**: Evenly distributes requests across available healthy servers
- **Automatic health checks**: Configurable interval (default 5 seconds)
- **Request-level retry logic**: Automatically retries failed requests on different healthy servers (up to 3 attempts)
- **Real-time failure detection**: Immediately re-checks server health when requests fail, doesn't wait for next health check cycle
- **YAML configuration**: Flexible configuration management via `config.yaml`
- **Health monitoring endpoint**: Real-time health status via `/healthz` API endpoint
- **Graceful failure handling**: Returns appropriate HTTP errors when all servers are down or all retries fail
- **Request logging**: Comprehensive logging of requests, health checks, retries, and server status changes
- **Concurrent health checking**: Non-blocking parallel health checks for optimal performance
- **Rate limiting**: Token bucket per client IP; configurable capacity and reset interval

## Usage

### Prerequisites

- Go 1.16 or higher
- Podman (for running backend servers)

#### Installing Podman

If you don't have Podman installed, you can install it using Homebrew:

```bash
brew install podman
```

For detailed installation instructions, visit the [official Podman installation guide](https://podman.io/getting-started/installation).

### Running the Application

**First, set up and start the backend servers using Podman:**
```bash
make up
```

This command will start the backend servers using Podman containers, then run the reverse proxy.

**Alternative manual steps:**

1. **Build the application:**
   ```bash
   make build
   ```

2. **Run the proxy:**
   ```bash
   make run
   ```

3. **Access the application:**
   ```bash
   # Test load balancing
   curl http://localhost:8080/hello
   
   # Check server health status  
   curl http://localhost:8080/healthz
   ```

### Testing Backend Servers

You can test individual backend servers:

```bash
make call-server1  # Test server on localhost:9001
make call-server2  # Test server on localhost:9002  
make call-server3  # Test server on localhost:9003
```

### Configuration

The application uses YAML configuration via `config.yaml`. Copy `config.example.yaml` to get started:

```bash
cp config.example.yaml config.yaml
```

Example configuration:
```yaml
listen: ":8080"
routes:
  - prefix: "/server1"
    target: "http://localhost:9001"
  - prefix: "/server2" 
    target: "http://localhost:9002"
  - prefix: "/server3"
    target: "http://localhost:9003"
healthCheckInterval: 5s
rateLimitTokenCapacity: 5
rateLimitResetInterval: 1m

# Advanced health check configuration
healthCheckFailureCodes: [502, 503, 504]  # HTTP codes indicating failure
healthCheckMaxRetries: 3                   # max retries before removing from rotation
healthCheckRetryDelay: 1s                  # initial delay with exponential backoff
healthCheckTimeout: 5s                     # timeout for health check requests
```

Config keys:
- `healthCheckInterval`: How often to run backend health checks (e.g., `3s`, `10s`). Default: `5s`.
- `rateLimitTokenCapacity`: Max tokens per client bucket. Default: `5`.
- `rateLimitResetInterval`: How often buckets reset to full capacity (e.g., `30s`, `1m`). Default: `1m`.

### Advanced Health Check Configuration

The load balancer supports sophisticated health check failure handling with exponential backoff and configurable retry policies:

| Configuration Key | Description | Default Value | Example Values |
|---|---|---|---|
| `healthCheckFailureCodes` | HTTP status codes that indicate server failure | `[502]` | `[502, 503, 504]` |
| `healthCheckMaxRetries` | Maximum retry attempts before removing server from rotation | `1` | `3`, `5` |
| `healthCheckRetryDelay` | Initial delay between retries (exponential backoff applied) | `1s` | `500ms`, `2s` |
| `healthCheckTimeout` | Timeout for individual health check requests | `5s` | `3s`, `10s` |

**Health Check Mechanism:**
- Servers are checked at `healthCheckInterval` (default: 5s)
- Failed servers retry with exponential backoff (1.5x multiplier)
- After `healthCheckMaxRetries` failures, servers are removed from rotation
- Servers returning non-JSON responses with HTTP 200-299 are considered healthy
- Custom failure codes can be configured via `healthCheckFailureCodes`

## How It Works

1. **Health Monitoring**: At a configurable interval (default 5s), the load balancer checks all backend servers concurrently
2. **Request Handling**: Incoming requests to `/hello` trigger the health-aware load balancer
3. **Healthy Server Selection**: Only healthy servers are considered for load balancing
4. **Round-robin Distribution**: Requests are distributed evenly among healthy servers using an atomic counter
5. **Request Forwarding**: The request is proxied to the selected healthy backend server
6. **Real-time Retry Logic**: If a request fails (server went down between health checks):
   - Immediately triggers a health check for the failed server
   - Automatically retries the request on the next healthy server (up to 3 attempts total)
   - Updates server status in real-time without waiting for the next health check cycle
7. **Failure Handling**: If all servers are unhealthy or all retries fail, returns HTTP 502 Bad Gateway
8. **Logging**: Comprehensive logging of health checks, server status changes, request routing, and retry attempts

### Advanced Health Check Behavior

The load balancer implements sophisticated failure handling with exponential backoff:

1. **Initial Failure**: When a server first fails a health check, it's marked unhealthy but remains in rotation for retry attempts
2. **Exponential Backoff**: Failed servers are retried with increasing delays:
   - First retry: after `healthCheckRetryDelay` (default: 1s)
   - Second retry: after 1.5x the previous delay (1.5s)
   - Third retry: after 1.5x the previous delay (2.25s)
   - And so on, with a 1.5x multiplier
3. **Server Rotation Removal**: After `healthCheckMaxRetries` failures (default: 1), the server is removed from rotation entirely
4. **Automatic Recovery**: When a previously failed server responds successfully, it's immediately restored to healthy status and added back to rotation
5. **Concurrent Checking**: Health checks run concurrently for all servers to minimize performance impact

### Server Rotation and Retry Behavior

The load balancer implements intelligent server rotation management:

**In-Rotation Status:**
- **Healthy servers**: Immediately available for load balancing
- **Failed servers (within retry limit)**: Remain in rotation but marked unhealthy, retried with exponential backoff
- **Exhausted servers**: Removed from rotation after exceeding `healthCheckMaxRetries`

**Retry Timeline Example** (with `healthCheckMaxRetries: 3`, `healthCheckRetryDelay: 1s`):
- **Failure 1**: Retry in 1s (attempt 1/3)
- **Failure 2**: Retry in 1.5s (attempt 2/3) 
- **Failure 3**: Retry in 2.25s (attempt 3/3)
- **Failure 4**: Server removed from rotation

**Recovery Behavior:**
- Servers are restored to full health status immediately upon successful response
- No gradual "warm-up" period - instant restoration to rotation
- Recovery can happen during scheduled health checks or immediate request-triggered checks

## Load Balancing Pattern

The load balancer uses a health-aware round-robin algorithm:
- **Only healthy servers receive traffic**
- If all servers are healthy: Request 1 → Server 1, Request 2 → Server 2, etc.
- If Server 2 is unhealthy: Request 1 → Server 1, Request 2 → Server 3, Request 3 → Server 1, etc.
- **Automatic recovery**: When unhealthy servers recover, they're automatically included in rotation

## API Endpoints

- **`/hello`** - Load-balanced endpoint that forwards requests to healthy backend servers
- **`/healthz`** - Health monitoring endpoint that returns JSON status of all backend servers

Example health check response:
```json
[
  {"url": "http://localhost:9001", "healthy": true},
  {"url": "http://localhost:9002", "healthy": false, "error": "connection refused"},
  {"url": "http://localhost:9003", "healthy": true}
]
```

## Example Output

```
2024/01/01 12:00:00 listening on :8080, forwarding based on config
2024/01/01 12:00:00 Health checks available at /healthz
2024/01/01 12:00:00 Starting health checks every 5s
2024/01/01 12:00:00 Rate limiting enabled: Token bucket with capacity 5 tokens, resets to full capacity every 1m0s
2024/01/01 12:00:00 Health check configuration: failure codes [502 503 504], max retries 3, retry delay 1s, timeout 5s
2024/01/01 12:00:00 Running initial health check...
2024/01/01 12:00:05 Request: forwarding /hello to http://localhost:9001
2024/01/01 12:00:06 Request: forwarding /hello to http://localhost:9002
2024/01/01 12:00:07 Info: Server http://localhost:9003 responded with HTTP 200 (non-JSON), treating as healthy
2024/01/01 12:00:08 Request: forwarding /hello to http://localhost:9003
2024/01/01 12:00:10 Request failed to http://localhost:9001: dial tcp 127.0.0.1:9001: connection refused (attempt 1/3)
2024/01/01 12:00:10 Server http://localhost:9001 marked unhealthy (attempt 1/3), next retry at 2024-01-01T12:00:11Z: dial tcp 127.0.0.1:9001: connection refused
2024/01/01 12:00:10 Request: forwarding /hello to http://localhost:9002 (attempt 2)
2024/01/01 12:00:15 CRITICAL: All servers are down! Load balancer entering failure mode.
2024/01/01 12:00:20 Server http://localhost:9001 recovered and is now healthy
2024/01/01 12:00:20 RECOVERY: Servers are back online! Load balancer operational again.
```

## Development

To modify the application:

1. Edit `main.go` to change backend servers or add new features
2. Clean and rebuild with `make clean && make build`
3. Restart the application with `make run`

### Additional Commands

- `make tidy` - Clean up Go module dependencies
- `make clean` - Remove build artifacts

### Completed Features ✅
- ✅ **Health checks** - Automatic monitoring and skipping of unhealthy servers
- ✅ **Health status endpoint** - `/healthz` API for monitoring server status
- ✅ **Configuration management** - YAML-based configuration system
- ✅ **Enhanced logging** - Comprehensive request and health check logging
- ✅ **Request-level retry logic** - Retries failed requests on other healthy servers
- ✅ **Rate limiting** - Token bucket per client IP

### Planned Features 🚧
- **Header modification** - Adding custom response headers, removing/modifying request headers  
- **Advanced metrics** - Prometheus metrics, request timing, throughput statistics
- **Multiple routing strategies** - Weighted round-robin, least connections, IP hash
- **SSL/TLS termination** - HTTPS support with certificate management


## License

This project is open source.
