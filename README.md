# Subbed

A YouTube video player with synchronized subtitles using Alpine.js, Go (Fiber), and SQLite.

## Features

- YouTube video embedding with custom subtitle support
- Synchronized subtitle display (SRT/VTT formats)
- Admin interface for managing videos and subtitles
- Docker support with volume persistence
- Built with Alpine.js and Go Fiber

## Quick Start

### Using Docker

```bash
docker pull ghcr.io/abdusco/subbed:latest
docker run -d \
  -p 3000:3000 \
  -e ADMIN_CREDENTIALS=admin:admin \
  -v ./data:/app/data \
  ghcr.io/abdusco/subbed:latest
```

### Using Docker Compose

1. Create a `docker-compose.yml`:
```yaml
version: '3.8'

services:
  subbed:
    image: ghcr.io/abdusco/subbed:latest
    ports:
      - "3000:3000"
    environment:
      - DATABASE_PATH=/app/data/subbed.db
      - ADMIN_CREDENTIALS=admin:admin
      - DEBUG=false
      - HOST=0.0.0.0  # Listen on all interfaces inside container
      - PORT=3000
    volumes:
      - ./data:/app/data
    restart: unless-stopped
```

2. Run:
```bash
docker-compose up -d
```

3. Access the application:
   - Main app: http://localhost:3000
   - Admin panel: http://localhost:3000/admin (default: admin/admin)

### Local Development

1. Install dependencies:
```bash
go mod download
```

2. Run in debug mode:
```bash
DEBUG=true ADMIN_CREDENTIALS=admin:admin go run .
```

3. Access:
   - Main app: http://localhost:3000
   - Admin panel: http://localhost:3000/admin

## Configuration

Environment variables:

- `DATABASE_PATH`: SQLite database file path (default: `./subbed.db`)
- `ADMIN_CREDENTIALS`: Admin credentials in format `username:password` (required)
- `DEBUG`: Enable debug mode to serve static files from filesystem (default: `false`)
- `HOST`: Interface to bind to (default: `127.0.0.1`). Use `0.0.0.0` to listen on all interfaces
- `PORT`: Port to listen on (default: `3000`)
- `LISTEN_ADDR`: Full listen address, overrides `HOST` and `PORT` if set (e.g., `0.0.0.0:8080`)

### Listen Address Examples

```bash
# Listen on localhost only (default)
./subbed

# Listen on all interfaces
HOST=0.0.0.0 ./subbed

# Change port
PORT=8080 ./subbed

# Bind to specific interface and port
HOST=192.168.1.100 PORT=8000 ./subbed

# Or use LISTEN_ADDR for full control
LISTEN_ADDR=0.0.0.0:8080 ./subbed
```

## Usage

### Adding Videos

1. Go to http://localhost:3000/admin
2. Log in with admin credentials
3. Add a new video with YouTube URL and title
4. Upload subtitle files (SRT or VTT format)

### Viewing Videos

Navigate to http://localhost:3000 and either:
- Enter a YouTube URL in the interface
- Use direct URL routing: `http://localhost:3000/https://youtube.com/watch?v=VIDEO_ID`

### API Endpoints

Get video and subtitle data:
```
GET /api/video?url=https://youtube.com/watch?v=VIDEO_ID
```

Response:
```json
{
  "video": {
    "id": 1,
    "original_url": "VIDEO_ID",
    "title": "Video Title"
  },
  "subtitles": [
    {
      "id": 1,
      "video_id": 1,
      "language": "en",
      "type": "srt",
      "content": "..."
    }
  ]
}
```

Admin API (requires basic auth):
- `GET /api/admin/videos` - List all videos with subtitles
- `POST /api/admin/videos` - Add new video
- `DELETE /api/admin/videos/:id` - Delete video
- `POST /api/admin/subtitles` - Upload subtitle file
- `DELETE /api/admin/subtitles/:id` - Delete subtitle

## Database Schema

### Videos Table
- `id`: INTEGER PRIMARY KEY
- `original_url`: TEXT (YouTube URL)
- `title`: TEXT

### Subtitles Table
- `id`: INTEGER PRIMARY KEY
- `video_id`: INTEGER (foreign key)
- `language`: TEXT (e.g., "en", "es")
- `type`: TEXT (always "srt")
- `content`: TEXT (subtitle content)

## Tech Stack

- **Frontend**: Alpine.js, Vanilla JavaScript, YouTube IFrame API
- **Backend**: Go 1.24, Fiber v2
- **Database**: SQLite3 (modernc.org/sqlite)
- **Query Builder**: goqu
- **Deployment**: Docker, Docker Compose

## Development

The application embeds static files in the binary for production. Set `DEBUG=true` to serve files from `./static` directory for development. Structured logging (slog) with JSON output is used throughout.
