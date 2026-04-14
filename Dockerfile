# Build stage
FROM golang:tip-trixie AS builder

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
FROM ubuntu:24.04


WORKDIR /root/

# Copy the binary from builder
COPY --from=builder /app/empty-xds .

EXPOSE 5000

CMD ["./empty-xds"]
