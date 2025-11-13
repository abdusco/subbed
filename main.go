package main

import (
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/basicauth"
	"github.com/gofiber/fiber/v2/middleware/filesystem"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/fiber/v2/middleware/recover"
)

//go:embed static/*
var staticFiles embed.FS

type Video struct {
	ID          int    `json:"id" db:"id"`
	OriginalURL string `json:"original_url" db:"original_url"`
	Title       string `json:"title" db:"title"`
}

type Subtitle struct {
	ID       int    `json:"id" db:"id"`
	VideoID  int    `json:"video_id" db:"video_id"`
	Language string `json:"language" db:"language"`
	Type     string `json:"type" db:"type"`
	Content  string `json:"content" db:"content"`
}

type VideoResponse struct {
	Video     Video      `json:"video"`
	Subtitles []Subtitle `json:"subtitles"`
}

// customErrorHandler handles all errors in a centralized way
func customErrorHandler(c *fiber.Ctx, err error) error {
	// Log the error
	slog.Error("Request error",
		"error", err,
		"path", c.Path(),
		"method", c.Method())

	return fiber.DefaultErrorHandler(c, err)
}

func main() {
	if err := run(); err != nil {
		slog.Error("Application failed to start", "error", err)
		os.Exit(1)
	}
}

func run() error {
	// Initialize structured logging
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	// Get environment variables
	dbPath := os.Getenv("DATABASE_PATH")
	if dbPath == "" {
		dbPath = "./subbed.db"
	}
	debug := os.Getenv("DEBUG") == "true"
	creds, err := newCredentialsFromEnvironment("ADMIN_CREDENTIALS")
	if err != nil {
		return fmt.Errorf("failed to parse admin credentials: %w", err)
	}

	// Initialize repository
	repo, err := NewRepository(dbPath)
	if err != nil {
		return fmt.Errorf("failed to initialize database: %w", err)
	}
	defer repo.Close()

	// Create Fiber app
	app := fiber.New(fiber.Config{
		Immutable:    true,
		ErrorHandler: customErrorHandler,
	})

	// Add recover middleware to handle panics
	app.Use(recover.New(recover.Config{
		EnableStackTrace: true,
	}))

	// Add logger middleware
	app.Use(logger.New(logger.Config{
		Format:     "[${time}] ${status} - ${method} ${path} ${latency}\n",
		TimeFormat: "15:04:05",
		TimeZone:   "Local",
	}))

	// Helper function to serve static files
	serveStaticFile := func(filePath string) fiber.Handler {
		return func(c *fiber.Ctx) error {
			if debug {
				return c.SendFile("./static/" + filePath)
			}
			content, err := staticFiles.ReadFile("static/" + filePath)
			if err != nil {
				return err
			}
			c.Set("Content-Type", "text/html")
			return c.Send(content)
		}
	}

	// Specific routes (registered first to take precedence)

	// Static files (register before other routes to take precedence)
	if debug {
		app.Static("/static", "./static")
	} else {
		staticFS, err := fs.Sub(staticFiles, "static")
		if err != nil {
			return fmt.Errorf("failed to load static files: %w", err)
		}
		app.Use("/static", filesystem.New(filesystem.Config{
			Root: http.FS(staticFS),
		}))
	}

	// Root route - serve index.html
	app.Get("/", serveStaticFile("index.html"))

	app.Get("/api/video", handleVideoRequest(repo))

	auth := basicAuthMiddleware(creds)
	app.Get("/admin", auth, serveStaticFile("admin.html"))

	adminAPI := app.Group("/api/admin", auth)
	adminAPI.Get("/videos", listVideos(repo))
	adminAPI.Post("/videos", addVideo(repo))
	adminAPI.Delete("/videos/:id", deleteVideo(repo))
	adminAPI.Post("/subtitles", uploadSubtitle(repo))
	adminAPI.Delete("/subtitles/:id", deleteSubtitle(repo))

	// Handle YouTube URL routing: /$youtubeURL redirects to /?url=$youtubeURL
	app.Get("/*", func(c *fiber.Ctx) error {
		_, ok := youtubeURLFromPath(string(c.Request().URI().PathOriginal()))
		if ok {
			return serveStaticFile("index.html")(c)
		}

		return c.Next()
	})

	// Root static file fallback (registered last)
	if debug {
		app.Static("/", "./static")
	} else {
		staticFS, err := fs.Sub(staticFiles, "static")
		if err != nil {
			slog.Error("Failed to load static files", "error", err)
			os.Exit(1)
		}
		app.Use("/", filesystem.New(filesystem.Config{
			Root: http.FS(staticFS),
		}))
	}

	slog.Info("Server starting", "port", 3000)
	if err := app.Listen(":3000"); err != nil {
		return fmt.Errorf("server failed to start: %w", err)
	}

	return nil
}

type Credentials struct {
	Username string
	Password string
}

func newCredentialsFromEnvironment(envVar string) (Credentials, error) {
	creds := os.Getenv(envVar)
	parts := strings.SplitN(creds, ":", 2)
	if len(parts) != 2 {
		return Credentials{}, fmt.Errorf("invalid credentials format in %q, expected username:password", envVar)
	}
	return Credentials{
		Username: parts[0],
		Password: parts[1],
	}, nil
}

func basicAuthMiddleware(creds Credentials) fiber.Handler {
	return basicauth.New(basicauth.Config{
		Users: map[string]string{
			creds.Username: creds.Password,
		},
	})
}

func youtubeURLFromPath(path string) (string, bool) {
	parts := strings.SplitN(path, "http", 2)
	if len(parts) != 2 {
		return "", false
	}
	urlStr := "http" + parts[1]

	if strings.HasPrefix(urlStr, "https:/") && !strings.HasPrefix(urlStr, "https://") {
		urlStr = strings.Replace(urlStr, "https:/", "https://", 1)
	} else if strings.HasPrefix(urlStr, "http:/") && !strings.HasPrefix(urlStr, "http://") {
		urlStr = strings.Replace(urlStr, "http:/", "http://", 1)
	}

	return urlStr, true
}

func youtubeVideoIDFromURL(urlStr string) (string, bool) {
	parsedURL, err := url.Parse(urlStr)
	if err == nil {
		// Extract video ID from different YouTube URL formats
		if strings.Contains(parsedURL.Host, "youtube.com") || strings.Contains(parsedURL.Host, "www.youtube.com") {
			// Standard format: youtube.com/watch?v=VIDEO_ID
			videoID := parsedURL.Query().Get("v")
			if videoID != "" {
				return videoID, true
			}
		} else if strings.Contains(parsedURL.Host, "youtu.be") {
			// Short format: youtu.be/VIDEO_ID
			videoID := strings.TrimPrefix(parsedURL.Path, "/")
			if videoID != "" {
				return videoID, true
			}
		}
	}

	return "", false
}

func handleVideoRequest(repo *Repository) fiber.Handler {
	return func(c *fiber.Ctx) error {
		ctx := c.Context()

		// Get the full path after /v/
		youtubeURL := c.Query("url")

		// Parse video ID
		videoID, ok := youtubeVideoIDFromURL(youtubeURL)
		if !ok {
			return fiber.NewError(fiber.StatusBadRequest, "Invalid YouTube URL")
		}

		// Look up video in database
		video, err := repo.GetVideoByURL(ctx, videoID)
		if err != nil {
			return fiber.NewError(fiber.StatusNotFound, "Video not found")
		}

		// Get subtitles for this video
		subtitles, err := repo.GetSubtitlesByVideoID(ctx, video.ID)
		if err != nil {
			return err
		}

		// Return response
		return c.JSON(VideoResponse{
			Video: Video{
				ID:          video.ID,
				OriginalURL: videoID,
				Title:       video.Title,
			},
			Subtitles: subtitles,
		})
	}
}

func listVideos(repo *Repository) fiber.Handler {
	return func(c *fiber.Ctx) error {
		ctx := c.Context()

		videos, err := repo.ListAllVideos(ctx)
		if err != nil {
			return err
		}

		return c.JSON(videos)
	}
}

func addVideo(repo *Repository) fiber.Handler {
	return func(c *fiber.Ctx) error {
		ctx := c.Context()

		var req struct {
			URL   string `json:"url"`
			Title string `json:"title"`
		}

		if err := c.BodyParser(&req); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "Invalid request")
		}

		id, err := repo.CreateVideo(ctx, req.URL, req.Title)
		if err != nil {
			return err
		}

		return c.JSON(fiber.Map{"id": id})
	}
}

func deleteVideo(repo *Repository) fiber.Handler {
	return func(c *fiber.Ctx) error {
		ctx := c.Context()

		id := c.Params("id")
		idInt, err := strconv.Atoi(id)
		if err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "Invalid ID")
		}

		err = repo.DeleteVideo(ctx, idInt)
		if err != nil {
			return err
		}
		return c.JSON(fiber.Map{"success": true})
	}
}

func uploadSubtitle(repo *Repository) fiber.Handler {
	return func(c *fiber.Ctx) error {
		ctx := c.Context()

		videoID := c.FormValue("video_id")
		videoIDInt, err := strconv.Atoi(videoID)
		if err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "Invalid video ID")
		}

		language := c.FormValue("language")
		fileType := c.FormValue("type")

		file, err := c.FormFile("file")
		if err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "No file uploaded")
		}

		// Read file content
		fileContent, err := file.Open()
		if err != nil {
			return err
		}
		defer fileContent.Close()

		content := make([]byte, file.Size)
		_, err = fileContent.Read(content)
		if err != nil {
			return err
		}

		contentStr := string(content)

		// Convert VTT to SRT if necessary
		if fileType == "vtt" {
			contentStr = vttToSRT(contentStr)
		}

		// Save to database (always as SRT)
		err = repo.CreateSubtitle(ctx, videoIDInt, language, "srt", contentStr)
		if err != nil {
			return err
		}

		return c.JSON(fiber.Map{"success": true})
	}
}

func deleteSubtitle(repo *Repository) fiber.Handler {
	return func(c *fiber.Ctx) error {
		ctx := c.Context()

		id := c.Params("id")
		idInt, err := strconv.Atoi(id)
		if err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "Invalid ID")
		}

		err = repo.DeleteSubtitle(ctx, idInt)
		if err != nil {
			return err
		}
		return c.JSON(fiber.Map{"success": true})
	}
}

func vttToSRT(vtt string) string {
	lines := strings.Split(vtt, "\n")
	var srtLines []string
	counter := 1
	skipHeader := true

	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])

		// Skip VTT header
		if skipHeader {
			if strings.HasPrefix(line, "WEBVTT") || line == "" {
				continue
			}
			skipHeader = false
		}

		// Check if line is a timestamp
		if strings.Contains(line, "-->") {
			// Add counter
			srtLines = append(srtLines, strconv.Itoa(counter))
			counter++

			// Convert timestamp format (remove millisecond dot to comma)
			line = strings.ReplaceAll(line, ".", ",")
			srtLines = append(srtLines, line)
		} else if line != "" {
			srtLines = append(srtLines, line)
		} else {
			srtLines = append(srtLines, "")
		}
	}

	return strings.Join(srtLines, "\n")
}
