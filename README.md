# Corridor

Corridor is a lightweight, configurable, and extensible API Gateway written in Go. It's designed to be the front door for your microservices, handling concerns like routing, load balancing, authentication, rate limiting, and more.

&nbsp;

Please note that Corridor is in active development, and while it is functional, it may not yet be suitable for production use. The project is evolving, and contributions are welcome!

&nbsp;

## Features

* **Dynamic Routing:** Route incoming requests to appropriate backend services based on path, host, and HTTP methods.
* **Load Balancing:** Distribute traffic across multiple instances of your backend services.
  * Strategies: Round Robin, Random, Least Connections.
  * **Health Checks:** Actively monitor the health of upstream service instances and automatically remove unhealthy instances from the load balancing pool.
    * Supports readiness and liveness probes.
* **Plugin Architecture:** Extend gateway functionality with a flexible plugin system.
* **Authentication:**
  * **JWT:** Validate JSON Web Tokens.
  * **API Key:** Secure access using API keys, with support for hashed key storage and dynamic loading from MongoDB.
  * **Rate Limiting:** Protect your services from abuse with configurable rate limits (per IP or per header value).
  * **Caching:** Cache responses from upstream services to improve performance and reduce load.
  * **Path Rewriting:** Modify request paths before forwarding them to upstream services.
  * **Request ID:** Ensure every request has a unique ID for tracing and logging.
  * **Logging:** Configurable request/response logging (JSON or text).
* **TLS Termination:** Handle HTTPS traffic by configuring SSL/TLS certificates.
* **Configuration Driven:** All aspects of the gateway are controlled via a YAML configuration file.
* **Graceful Shutdown:** Ensures in-flight requests are handled before shutting down.

&nbsp;

## Getting Started

&nbsp;

1. **Prerequisites:**
   * Go (version 1.23+ recommended)
   * (Optional) MongoDB if using MongoDB for API key storage.

2. **Configuration:**
   * Corridor uses a YAML file for configuration (e.g., `config.yaml`).
   * A detailed sample configuration file, `config-sample.yaml`, is provided in the `app/` directory. It showcases all available options with explanations.

3. **Running Corridor:**

    * For running Corridor using containerized environments, refer to the [Running Corridor with Containers](#running-corridor-with-containers) section below.
    * For running Corridor directly from source, ensure you have Go installed and run:

    ```bash
    ./corridor -config /path/to/your/config.yaml
    ```

&nbsp;

## Configuration (`config.yaml`)

The gateway's behavior is entirely defined by a YAML configuration file. Below is an overview of the main sections. Refer to `config-sample.yaml` for detailed examples and all options.

&nbsp;

### 1. `global`

This section contains settings that apply to the entire gateway.

```yaml
global:
  port: 8080
  log_level: "info" # debug, info, warn, error, fatal
  enable_https: false
  default_timeout_ms: 10000
  proxy_headers:
    - "X-Request-ID"
    - "X-Forwarded-For"
  # Server timeouts (read_timeout_sec, write_timeout_sec, etc.)
  # Transport timeouts for upstream connections (transport_dial_timeout_sec, etc.)
  # fail_on_service_init_error: false
```

Key options include:

* `port`: Listening port for HTTP (and HTTPS if enabled).
* `log_level`: Verbosity of gateway logs.
* `enable_https: true/false`: Enables HTTPS. Requires `tls` section.
* `default_timeout_ms`: Default timeout for upstream requests.
* `proxy_headers`: Standard headers like `X-Forwarded-For` to manage.
* Server and Transport Timeouts: Fine-grained control over connection timeouts.
* `fail_on_service_init_error`: Determines if gateway startup fails if a service can't initialize.

&nbsp;

### 2. `services`

Define your backend services that Corridor will proxy requests to.

```yaml
services:
  - name: "my-backend-service"
    upstream_urls:
      - "http://localhost:8081"
      - "http://service1-instance2:8081"
    load_balancing:
      strategy: "round-robin" # round-robin, random, least-connections
      health_checks:
        ready: "/health/ready" # Prioritized if available
        live: "/health/live"   # Fallback if ready not defined
        interval: 15           # Seconds
        timeout: 5             # Seconds
        unhealthy_threshold: 3
        healthy_threshold: 2
```

* `name`: A unique name for the service.
* `upstream_urls`: A list of URLs for the instances of this service.
* `load_balancing`:
  * `strategy`: How to pick an upstream instance.
  * `health_checks`: Configuration for active health monitoring of upstream instances.

&nbsp;

### 3. `routes`

Define rules to match incoming requests and forward them to the configured services.

```yaml
routes:
  - id: "unique-route-id"
    paths:
      - "/api/v1/users/*" # Matches /api/v1/users/ and anything under it
      - "/profile"        # Exact match
    methods: ["GET", "POST"] # Optional: If empty, matches all methods
    host: "api.example.com"  # Optional: Exact or wildcard (*.example.com)
    service: "my-backend-service" # Name of the service to route to
    plugins: # Optional: List of plugins to apply to this route
      - name: "rate-limiting"
        config:
          requests_per_minute: 60
      - name: "path-rewrite"
        config:
          regex: "^/api/v1/users(/.*)$"
          replacement: "/users$1"
```

* `id`: A unique identifier for the route.
* `paths`: List of URL path patterns. Supports exact matches and trailing `*` for prefix matching.
* `methods`: List of HTTP methods (case-insensitive).
* `host`: Host header to match (exact or wildcard).
* `service`: The name of the service (from the `services` section) to forward the request to.
* `plugins`: A list of plugin configurations specific to this route.

&nbsp;

### 4. `plugins`

This section allows for global configuration of plugins. Route-specific plugin configurations can override these global settings.

```yaml
plugins:
  authentication:
    provider: "none" # Default: none, jwt, api-key

    # JWT Settings (if provider: "jwt")
    # jwt_secret: "your-hmac-secret"
    # jwt_public_key_file: "/path/to/public.pem"
    # jwt_algorithm: "HS256" # or RS256, ES256, etc.
    # jwt_issuer: "auth.example.com"
    # jwt_audience: "api.example.com"
    # token_header_name: "Authorization"
    # token_header_scheme: "Bearer"

    # API Key Settings (if provider: "api-key")
    # api_key_header_name: "X-API-Key"

    # MongoDB for API Keys (Recommended for production API Key provider)
    # mongodb_uri: "mongodb://localhost:27017"
    # mongodb_database: "corridor_db"
    # mongodb_collection_apikeys: "api_keys"
    # mongodb_timeout_ms: 5000
    # mongodb_apikeys_refresh_interval_sec: 300 # How often to refresh keys from DB

    # YAML-based API Keys (Fallback or for simpler setups)
    # api_keys:
    #   - client_id: "client1"
    #     lookup_key: "sha256_hex_of_plaintext_key"
    #     hashed_value: "bcrypt_hash_of_plaintext_key"

  rate-limiting:
    strategy: "ip-address" # or "header"
    # header_name: "X-Client-ID" # if strategy is "header"
    # requests_per_minute: 1000 # Default global limit

  caching:
    enabled: true
    default_ttl_seconds: 300

  logging:
    format: "json" # or "text"

  # request-id:
  #   header_name: "X-Request-ID"
```

* **`authentication`**:
  * `provider`: `jwt`, `api-key`, or `none`.
  * JWT specific settings: `jwt_secret`, `jwt_public_key_file`, `jwt_algorithm`, `jwt_issuer`, `jwt_audience`, `token_header_name`, `token_header_scheme`.
  * API Key specific settings: `api_key_header_name`.
  * MongoDB settings for API keys: `mongodb_uri`, `mongodb_database`, etc., for dynamic and secure key storage.
  * `api_keys` list for YAML-based (hashed) API key definitions.
* **`rate-limiting`**: `strategy` (`ip-address` or `header`), `header_name`, `requests_per_minute`.
* **`caching`**: `enabled`, `default_ttl_seconds`.
* **`logging`**: `format` (`json` or `text`).
* **`request-id`**: `header_name`.

&nbsp;

### 5. `tls` (Optional)

```yaml
# Configure SSL/TLS certificates if `global.enable_https` is true. Supports multiple certificates for different hostnames (SNI).

```yaml
tls:
  certificates:
    - hostname: "api.example.com"
      cert_file: "/path/to/api.example.com.crt"
      key_file: "/path/to/api.example.com.key"
    # - hostname: "*.anotherdomain.com"
    #   cert_file: "/path/to/anotherdomain.com.crt"
    #   key_file: "/path/to/anotherdomain.com.key"
```

&nbsp;

## Plugin System

Corridor's plugin system allows you to modify requests and responses or implement custom logic at various stages of the request lifecycle. Plugins are applied in the order they are defined in a route's configuration.

Currently available plugins:

* **Authentication (`authentication`):** Secures your APIs using JWTs or API Keys.
* **Rate Limiting (`rate-limiting`):** Protects services from being overwhelmed.
* **Caching (`caching`):** Caches upstream responses.
* **Path Rewriting (`path-rewrite`):** Modifies the request path using regex.
* **Request ID (`request-id`):** Ensures each request has a unique identifier.
* **Logging (`logging`):** Provides structured request logging (though this is more of a core feature configured under plugins).

&nbsp;

## API Key Generation (Built-in Command)

If you are using the API Key authentication provider with hashed keys (recommended), Corridor includes a built-in command to help you generate the necessary hashes.

Run the following command:

```bash
./corridor keygen "your_desired_plaintext_api_key"
```

*Ensure the Corridor binary is in your PATH or use the direct path to it. Quote the API key if it contains spaces or special characters.*

This tool will output:

1. A **plaintext API key** (to give to your client).
2. A **`lookup_key`** (SHA256 hash of the plaintext key).
3. A **`hashed_value`** (bcrypt hash of the plaintext key).

The `lookup_key` and `hashed_value` are what you store in your `config.yaml` (under `plugins.authentication.api_keys`) or in your MongoDB collection for API keys. Corridor never stores the plaintext API key.

Example:

```bash
./corridor keygen "your_desired_plaintext_api_key"

--- Generated API Key Details ---
Plaintext API Key: your_desired_plaintext_api_key
  Lookup Key (SHA256 Hex for 'lookup_key' field): your_generated_sha256_hex_string
  Hashed Value (bcrypt for 'hashed_value' field): $2a$10$your_generated_bcrypt_hash_string

Instructions:
1. Provide the 'Plaintext API Key' to your client. Corridor does NOT store this.
2. Store the 'Lookup Key' and 'Hashed Value' in your Corridor configuration (e.g., config.yaml or MongoDB).
```

&nbsp;

## Running Corridor with Containers

Corridor can be easily run using Docker, Docker Compose, or deployed to Kubernetes. The official Docker image is available on Docker Hub: `cloudresty/corridor:latest`.

You will need a `config.yaml` file for Corridor to run.

&nbsp;

### 1. Using Docker

**a) Mounting `config.yaml` from the host (Recommended for local development/testing):**

Create your `config.yaml` file in a directory on your host machine, for example, in `./corridor-config/config.yaml`.

```bash
# Assuming your config.yaml is in ./corridor-config/config.yaml
# and you want to map port 8080 on your host to port 8080 in the container.
docker run -d --name corridor \
  -p 8080:8080 \
  -v $(pwd)/corridor-config:/corridor/config \
  cloudresty/corridor:latest -config /corridor/config/config.yaml
```

* `-d`: Run in detached mode.
* `--name corridor`: Assign a name to the container.
* `-p 8080:8080`: Map port 8080 of the host to port 8080 of the container (adjust if your `config.yaml` uses a different port).
* `-v $(pwd)/corridor-config:/corridor/config`: Mounts the `./corridor-config` directory from your current host path into `/corridor/config` inside the container. Corridor will then look for `/corridor/config/config.yaml`.
* `cloudresty/corridor:latest`: The Docker image.
* `-config /corridor/config/config.yaml`: Tells Corridor inside the container where to find the config file.

**b) Building a custom Docker image with `config.yaml` embedded (Less flexible for config changes):**

Create a `Dockerfile`:

```dockerfile
FROM cloudresty/corridor:latest

# Copy your local config.yaml into the image
COPY ./my-corridor-config/config.yaml /corridor/config/config.yaml

# The default command in the base image should already point to /corridor/config.yaml
# If not, or if you place it elsewhere, you might need to adjust the CMD or ENTRYPOINT.
# The base image likely uses CMD ["/corridor/corridor", "-config", "/corridor/config/config.yaml"] or similar.
```

Then build and run:

```bash
docker build -t my-custom-corridor .
docker run -d --name corridor -p 8080:8080 my-custom-corridor
```

&nbsp;

### 2. Using Docker Compose

Create a `docker-compose.yaml` file:

```yaml
version: '3.8'

services:
  corridor:
    image: cloudresty/corridor:latest # Make sure you use a specific version in production and not just 'latest' which can change over time the behavior of your service.
    container_name: corridor-gateway
    ports:
      - "80:8080" # HostPort:ContainerPort (adjust if your config uses a different port)
    volumes:
      - ./config/corridor/config.yaml:/corridor/config/config.yaml # Mounts ./config/corridor/config.yaml on host to /corridor/config/config.yaml in container
    command: ["/corridor/corridor", "-config", "/corridor/config/config.yaml"] # Tells Corridor where to find the config
    restart: unless-stopped
```

Place your `config.yaml` inside a `./config/corridor/` directory relative to your `docker-compose.yaml`.

Then run:

```bash
docker compose up -d
```

To stop:

```bash
docker compose down
```

&nbsp;

### 3. Deploying to Kubernetes

Here's a basic example of deploying Corridor to Kubernetes.

**a) Create a `ConfigMap` for your `config.yaml`:**

```bash
# Replace ./path/to/your/config.yaml with the actual path to your configuration file
kubectl create configmap corridor-config --from-file=config.yaml=./path/to/your/config.yaml
```

**b) Create a `Deployment` and `Service`:**

Create a file named `corridor-deployment.yaml`:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: corridor-deployment
  labels:
    app: corridor
spec:
  replicas: 2 # Adjust as needed
  selector:
    matchLabels:
      app: corridor
  template:
    metadata:
      labels:
        app: corridor
    spec:
      containers:
      - name: corridor
        image: cloudresty/corridor:latest
        args: ["-config", "/corridor/config/config.yaml"]
        ports:
        - containerPort: 8080 # Port Corridor listens on (from your config.yaml)
        volumeMounts:
        - name: config-volume
          mountPath: /corridor/config # Mount the ConfigMap content here
      volumes:
      - name: config-volume
        configMap:
          name: corridor-config # Name of the ConfigMap created earlier
---
apiVersion: v1
kind: Service
metadata:
  name: corridor-service
spec:
  selector:
    app: corridor
  ports:
    - protocol: TCP
      port: 80         # Port the service will be exposed on within the cluster
      targetPort: 8080 # Port the Corridor container is listening on
  type: LoadBalancer   # Or NodePort, ClusterIP depending on your needs
```

Apply the deployment:

```bash
kubectl apply -f corridor-deployment.yaml
```

This example uses a `LoadBalancer` service type, which might provision an external IP depending on your Kubernetes cloud provider. Adjust the service type and ports as needed for your environment.

&nbsp;

## Development & Contribution

Corridor is an evolving project. Contributions are welcome! Please see the [CONTRIBUTING.md](CONTRIBUTING.md) file for guidelines.

---
&nbsp;

*This README provides a general overview. Always refer to `config-sample.yaml` for the most up-to-date and detailed configuration options.*

&nbsp;

---

Made with ♥️ by [Cloudresty](https://cloudresty.com).
