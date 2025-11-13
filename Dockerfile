# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /app

# Copy go mod files
COPY go.mod go.sum* ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the application without CGO
RUN CGO_ENABLED=0 GOOS=linux go build -a -ldflags="-s -w" -o subbed .

# Runtime stage
FROM alpine:latest

WORKDIR /app

# Install runtime dependencies
RUN apk add --no-cache ca-certificates

# Copy the binary from builder
COPY --from=builder /app/subbed .

# Expose port
EXPOSE 3000

# Run the application
CMD ["./subbed"]
