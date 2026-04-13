# Build stage
FROM docker.ops.iszn.cz/szn-image/golang-build:latest AS builder

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum* ./

# Download dependencies
RUN go mod download || true

# Copy source code
COPY . .

# Build the application
RUN go build -o empty-xds .

# Runtime stage
FROM docker.ops.iszn.cz/baseimage/ubuntu:noble


WORKDIR /root/

# Copy the binary from builder
COPY --from=builder /app/empty-xds .

EXPOSE 5000

CMD ["./empty-xds"]
