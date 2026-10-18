package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

//go:embed static/*
var embeddedStatic embed.FS

const (
	maxRequestBody  = "4M"
	shutdownTimeout = 10 * time.Second
)

// adminOnlyFile is served through the admin route only. It must never be reachable via the public static handler.
const adminOnlyFile = "admin.html"

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

type Credentials struct {
	Username string
	Password string
}

func main() {
	if err := run(); err != nil {
		slog.Error("Application failed to start", "error", err)
		os.Exit(1)
	}
}

// debugEnabled reports whether DEBUG is set to a true value ("1", "true", "t", ...).
func debugEnabled() bool {
	enabled, _ := strconv.ParseBool(os.Getenv("DEBUG"))
	return enabled
}

func newLogger(debug bool) *slog.Logger {
	options := &slog.HandlerOptions{Level: slog.LevelInfo}
	if debug {
		return slog.New(slog.NewTextHandler(os.Stdout, options))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, options))
}

func listenAddress() string {
	if addr := os.Getenv("LISTEN_ADDR"); addr != "" {
		return addr
	}
	host := os.Getenv("HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	return host + ":" + port
}

// staticAssets returns the web files: read from ./static in debug mode so edits show on reload, otherwise
// the copy embedded in the binary.
func staticAssets(debug bool) (fs.FS, error) {
	if debug {
		return os.DirFS("static"), nil
	}
	assets, err := fs.Sub(embeddedStatic, "static")
	if err != nil {
		return nil, fmt.Errorf("failed to load static files: %w", err)
	}
	return assets, nil
}

func run() error {
	debug := debugEnabled()
	logger := newLogger(debug)
	slog.SetDefault(logger)

	dbPath := os.Getenv("DATABASE_PATH")
	if dbPath == "" {
		dbPath = "./subbed.db"
	}

	creds, err := newCredentialsFromEnvironment("ADMIN_CREDENTIALS")
	if err != nil {
		return fmt.Errorf("failed to parse admin credentials: %w", err)
	}

	repo, err := NewRepository(dbPath)
	if err != nil {
		return fmt.Errorf("failed to initialize database: %w", err)
	}
	defer repo.Close()

	assets, err := staticAssets(debug)
	if err != nil {
		return err
	}

	sessionKey, err := sessionKeyFromEnvironment("SESSION_SECRET", logger)
	if err != nil {
		return err
	}

	e := newServer(serverConfig{repo: repo, assets: assets, creds: creds, sessionKey: sessionKey, debug: debug, logger: logger})

	addr := listenAddress()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("Listening", "addr", addr, "debug", debug)
		serverErr <- e.Start(addr)
	}()

	select {
	case err := <-serverErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("server failed to start: %w", err)
		}
		return nil
	case <-ctx.Done():
		logger.Info("Shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return e.Shutdown(shutdownCtx)
	}
}

type serverConfig struct {
	repo   *Repository
	assets fs.FS
	creds  Credentials
	// sessionKey signs the session cookies
	sessionKey []byte
	debug      bool
	logger     *slog.Logger
	oembed     *YoutubeOEmbed
}

// newServer builds the HTTP server with all routes and middleware.
func newServer(cfg serverConfig) *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.HTTPErrorHandler = jsonErrorHandler(cfg.logger)
	// c.File serves from here
	e.Filesystem = cfg.assets

	e.Use(requestLogger(cfg.logger))
	e.Use(middleware.RecoverWithConfig(middleware.RecoverConfig{
		LogErrorFunc: func(c echo.Context, err error, stack []byte) error {
			cfg.logger.Error("Panic", "error", err, "path", c.Request().URL.Path, "stack", string(stack))
			return err
		},
	}))
	e.Use(middleware.BodyLimit(maxRequestBody))

	session := newSessions(cfg.creds, cfg.sessionKey)
	adminPage := session.requireSession(true)
	adminAPI := session.requireSession(false)

	e.GET("/", servePage("index.html"))
	e.GET("/api/video", handleVideoRequest(cfg.repo))

	e.GET(loginPath, session.loginPage(servePage("login.html")))
	e.POST("/api/login", session.handleLogin, loginRateLimit())
	e.POST("/api/logout", session.handleLogout)

	e.GET("/admin", servePage(adminOnlyFile), adminPage)
	// the service worker must live outside /static so its scope can cover /admin
	e.GET("/admin-sw.js", servePage("admin-sw.js"), quiet)

	admin := e.Group("/api/admin", adminAPI)
	admin.GET("/videos", listVideos(cfg.repo))
	admin.POST("/videos", addVideo(cfg.repo))
	admin.PATCH("/videos/:id", updateVideo(cfg.repo))
	admin.DELETE("/videos/:id", deleteVideo(cfg.repo))
	admin.POST("/subtitles", uploadSubtitle(cfg.repo))
	admin.DELETE("/subtitles/:id", deleteSubtitle(cfg.repo))
	if cfg.oembed == nil {
		cfg.oembed = NewYoutubeOEmbed(nil, "")
	}
	admin.GET("/oembed", handleOEmbed(cfg.oembed))

	e.GET("/static/*", serveStatic(cfg.debug), quiet)
	// the player is addressed as /<youtube url>
	e.GET("/*", func(c echo.Context) error {
		if _, ok := youtubeURLFromPath(c.Request().URL.EscapedPath()); !ok {
			return echo.ErrNotFound
		}
		return servePage("index.html")(c)
	})

	return e
}

// servePage serves one file from the server's assets and makes browsers revalidate it, so a new version is picked up.
func servePage(name string) echo.HandlerFunc {
	return func(c echo.Context) error {
		c.Response().Header().Set(echo.HeaderCacheControl, "no-cache")
		return c.File(name)
	}
}

// serveStatic serves files under /static, except those that are reserved for authenticated routes.
func serveStatic(debug bool) echo.HandlerFunc {
	return func(c echo.Context) error {
		name := path.Clean("/" + c.Param("*"))[1:]
		if !fs.ValidPath(name) || name == adminOnlyFile {
			return echo.ErrNotFound
		}
		if debug {
			c.Response().Header().Set(echo.HeaderCacheControl, "no-cache")
		}
		return c.File(name)
	}
}

// requestLogger logs every request with slog.
func requestLogger(logger *slog.Logger) echo.MiddlewareFunc {
	return middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		LogMethod:    true,
		LogURI:       true,
		LogStatus:    true,
		LogLatency:   true,
		LogRemoteIP:  true,
		LogUserAgent: true,
		LogError:     true,
		// let the error handler run first so the logged status is the one the client received
		HandleError: true,
		LogValuesFunc: func(c echo.Context, v middleware.RequestLoggerValues) error {
			if c.Get(quietKey) == true && v.Status < 400 {
				return nil
			}
			attrs := []any{
				"method", v.Method,
				"status", v.Status,
				"path", v.URI,
				"duration", v.Latency.String(),
				"ip", v.RemoteIP,
				"user_agent", v.UserAgent,
			}
			level := slog.LevelInfo
			switch {
			case v.Status >= 500:
				level = slog.LevelError
				if v.Error != nil {
					attrs = append(attrs, "error", v.Error.Error())
				}
			case v.Status >= 400:
				level = slog.LevelWarn
			}
			logger.Log(c.Request().Context(), level, "HTTP request", attrs...)
			return nil
		},
	})
}

// quietKey marks a request that the request logger should skip when it succeeds.
const quietKey = "quiet"

// quiet keeps successful requests to a route out of the access log. Failures are still logged.
func quiet(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		c.Set(quietKey, true)
		return next(c)
	}
}

// errorResponse is the body of every API error.
type errorResponse struct {
	Error string `json:"error"`
}

// jsonErrorHandler answers errors with {"error": "message"}. Unexpected errors are reported as a generic
// 500 and are logged, with their details, by the request logger.
func jsonErrorHandler(logger *slog.Logger) echo.HTTPErrorHandler {
	return func(err error, c echo.Context) {
		if c.Response().Committed {
			return
		}
		code := http.StatusInternalServerError
		message := http.StatusText(code)
		var httpError *echo.HTTPError
		if errors.As(err, &httpError) {
			code = httpError.Code
			message = http.StatusText(code)
			if text, ok := httpError.Message.(string); ok && text != "" {
				message = text
			}
		}
		var sendErr error
		if c.Request().Method == http.MethodHead {
			sendErr = c.NoContent(code)
		} else {
			sendErr = c.JSON(code, errorResponse{Error: message})
		}
		if sendErr != nil {
			logger.Error("Failed to send error response", "error", sendErr)
		}
	}
}

func newCredentialsFromEnvironment(envVar string) (Credentials, error) {
	username, password, ok := strings.Cut(os.Getenv(envVar), ":")
	if !ok || username == "" || password == "" {
		return Credentials{}, fmt.Errorf("invalid credentials format in %q, expected username:password", envVar)
	}
	return Credentials{Username: username, Password: password}, nil
}
