#
# Builder Stage
#

# Golang image
FROM    golang:1.24-bookworm AS builder

RUN     set -x && \
            DEBIAN_FRONTEND=noninteractive \
            apt-get update && \
            apt-get install -y \
                ca-certificates

# Set the Current Working Directory inside the container
WORKDIR /corridor

ENV     CGO_ENABLED=0

# Copy all required files
COPY    app/ .

# Go Dependencies
RUN     go clean -modcache
RUN     go mod tidy -compat=1.24

# Build the Go app
RUN     go build -o corridor .

#
# Final Image
#

# Start fresh from a smaller image
FROM    debian:bookworm-slim

# Copy the output from builder stage
COPY    --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY    --from=builder /corridor/corridor /corridor/corridor

# Set the Current Working Directory inside the container
WORKDIR /corridor
RUN     rm -f config.yaml

EXPOSE  8080

# Execute the application when the container starts
CMD     ["/corridor/corridor"]