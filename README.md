# Go Reverse Proxy Load Balancer

A simple round-robin load balancer written in Go that distributes incoming HTTP requests across multiple backend servers.

## Features

- **Round-robin load balancing**: Distributes requests evenly across backend servers
- **Dynamic routing**: Each request is routed to the next server in sequence
- **Request logging**: Logs each request with counter and destination server
- **Simple configuration**: Easy to modify backend server URLs

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
   curl http://localhost:8080/hello
   ```

### Testing Backend Servers

You can test individual backend servers:

```bash
make call-server1  # Test server on localhost:9001
make call-server2  # Test server on localhost:9002  
make call-server3  # Test server on localhost:9003
```

### Configuration

The backend servers are configured in the `main.go` file:

```go
targets := []string{
    "http://localhost:9001",
    "http://localhost:9002", 
    "http://localhost:9003",
}
```


## How It Works

1. **Request Handling**: Each incoming request to `/hello` triggers the load balancer
2. **Counter Increment**: A request counter is incremented for each request
3. **Server Selection**: The counter is used with modulo operation to select the next backend server
4. **Request Forwarding**: The request is proxied to the selected backend server
5. **Logging**: Request details are logged including the counter and destination server

## Load Balancing Pattern

The load balancer uses a simple round-robin algorithm:
- Request 1 → Server 1 (localhost:9001)
- Request 2 → Server 2 (localhost:9002) 
- Request 3 → Server 3 (localhost:9003)
- Request 4 → Server 1 (localhost:9001)
- And so on...

## Example Output

```
2024/01/01 12:00:00 listening on :8080, forwarding -> :900X
2024/01/01 12:00:05 Request 1: forwarding to http://localhost:9001
2024/01/01 12:00:06 Request 2: forwarding to http://localhost:9002
2024/01/01 12:00:07 Request 3: forwarding to http://localhost:9003
2024/01/01 12:00:08 Request 4: forwarding to http://localhost:9001
```

## Development

To modify the application:

1. Edit `main.go` to change backend servers or add new features
2. Clean and rebuild with `make clean && make build`
3. Restart the application with `make run`

### Additional Commands

- `make tidy` - Clean up Go module dependencies
- `make clean` - Remove build artifacts

## License

This project is open source.
