# ==============================================================================
# STAGE 1: COMPILATION & BUILD OPTIMIZATION
# ==============================================================================
FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git

WORKDIR /app

# Download dependencies separately to leverage Docker layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy the entire workspace
COPY . .

# Compile with optimization flags (-s -w) to strip symbols and debug details
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o gateway-bin main.go balancer.go

# ==============================================================================
# STAGE 2: HIGH-PERFORMANCE MINIMAL RUNTIME
# ==============================================================================
FROM alpine:latest

RUN apk --no-cache add ca-certificates

WORKDIR /root/

# Copy the optimized binary and standard configuration file from builder
COPY --from=builder /app/gateway-bin .
COPY --from=builder /app/gateway.yaml .

# Expose standard gateway port
EXPOSE 8080

CMD ["./gateway-bin"]
