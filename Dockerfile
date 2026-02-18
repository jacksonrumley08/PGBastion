# Build stage
FROM golang:1.24-alpine AS builder

RUN apk add --no-cache git

WORKDIR /src

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Build binary
COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE}" \
    -o /pgbastion ./cmd/pgbastion/

# Runtime stage
FROM alpine:3.19

RUN apk add --no-cache \
    ca-certificates \
    tzdata \
    # For VIP management (optional - only needed if using VIP feature)
    iproute2 \
    # For health checks
    curl

# Create non-root user
RUN addgroup -g 1000 pgbastion && \
    adduser -u 1000 -G pgbastion -s /bin/sh -D pgbastion

# Create directories
RUN mkdir -p /etc/pgbastion /var/lib/pgbastion/raft && \
    chown -R pgbastion:pgbastion /var/lib/pgbastion

COPY --from=builder /pgbastion /usr/local/bin/pgbastion

# Copy example config
COPY config/pgbastion.yaml.example /etc/pgbastion/pgbastion.yaml.example

# Default config location
ENV PGBASTION_CONFIG=/etc/pgbastion/pgbastion.yaml

# Expose ports
# 5432 - Primary proxy (PostgreSQL writes)
# 5433 - Replica proxy (PostgreSQL reads)
# 8080 - REST API
# 9090 - Prometheus metrics
# 8300 - Raft consensus
# 7946 - Memberlist discovery
EXPOSE 5432 5433 8080 9090 8300 7946

# Health check
HEALTHCHECK --interval=10s --timeout=5s --start-period=30s --retries=3 \
    CMD curl -sf http://localhost:8080/health || exit 1

# Note: For VIP management, container must run with:
#   --cap-add=NET_ADMIN --cap-add=NET_RAW --network=host
# Without these, VIP features will be disabled.

USER pgbastion

ENTRYPOINT ["/usr/local/bin/pgbastion"]
CMD ["start", "--config", "/etc/pgbastion/pgbastion.yaml"]
