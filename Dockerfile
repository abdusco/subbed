# Build stage
FROM golang:1.27-alpine AS builder

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

# Install runtime dependencies: ffmpeg cuts the audio for transcription, and yt-dlp needs a JavaScript runtime
# for YouTube. yt-dlp itself is downloaded on first start into the data directory, so it can update itself.
RUN apk add --no-cache ca-certificates ffmpeg nodejs

# Copy the binary from builder
COPY --from=builder /app/subbed .

# Listen on all interfaces, or the published port can't reach the app
ENV HOST=0.0.0.0

# Expose port
EXPOSE 3000

# Run the application
CMD ["./subbed"]
