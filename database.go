package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/doug-martin/goqu/v9"
	_ "github.com/doug-martin/goqu/v9/dialect/sqlite3"
	_ "modernc.org/sqlite"
)

// Repository handles all database operations
type Repository struct {
	db *goqu.Database
}

// VideoWithSubs represents a video with its subtitles
type VideoWithSubs struct {
	Video
	Subtitles []Subtitle `json:"subtitles"`
}

// NewRepository creates a new repository instance
func NewRepository(dbPath string) (*Repository, error) {
	sqlDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Set good SQLite pragmas for performance and reliability
	pragmas := []string{
		"PRAGMA journal_mode=WAL",            // Write-Ahead Logging for better concurrency
		"PRAGMA synchronous=NORMAL",          // Balanced durability/performance
		"PRAGMA cache_size=-64000",           // 64MB cache
		"PRAGMA busy_timeout=5000",           // 5 second timeout for locked database
		"PRAGMA foreign_keys=ON",             // Enforce foreign key constraints
		"PRAGMA temp_store=MEMORY",           // Store temp tables in memory
		"PRAGMA mmap_size=268435456",         // 256MB memory-mapped I/O
		"PRAGMA page_size=4096",              // 4KB page size (must be set before DB creation)
		"PRAGMA auto_vacuum=INCREMENTAL",     // Incremental auto-vacuum
		"PRAGMA journal_size_limit=67108864", // 64MB journal size limit
		"PRAGMA wal_autocheckpoint=1000",     // Checkpoint every 1000 pages
	}

	for _, pragma := range pragmas {
		if _, err := sqlDB.Exec(pragma); err != nil {
			return nil, fmt.Errorf("failed to set pragma %s: %w", pragma, err)
		}
	}

	db := goqu.New("sqlite3", sqlDB)

	repo := &Repository{db: db}
	if err := repo.initDB(); err != nil {
		return nil, fmt.Errorf("failed to initialize database: %w", err)
	}

	return repo, nil
}

// Close closes the database connection
func (r *Repository) Close() error {
	if sqlDB, ok := r.db.Db.(*sql.DB); ok {
		return sqlDB.Close()
	}
	return nil
}

// initDB creates the database tables if they don't exist
func (r *Repository) initDB() error {
	sqlDB, ok := r.db.Db.(*sql.DB)
	if !ok {
		return fmt.Errorf("failed to get sql.DB instance")
	}

	// Create videos table
	_, err := sqlDB.Exec(`
		CREATE TABLE IF NOT EXISTS videos (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			original_url TEXT NOT NULL UNIQUE,
			title TEXT NOT NULL
		)
	`)
	if err != nil {
		return fmt.Errorf("failed to create videos table: %w", err)
	}

	// Create subtitles table
	_, err = sqlDB.Exec(`
		CREATE TABLE IF NOT EXISTS subtitles (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			video_id INTEGER NOT NULL,
			language TEXT NOT NULL,
			type TEXT NOT NULL,
			content TEXT NOT NULL,
			FOREIGN KEY (video_id) REFERENCES videos(id) ON DELETE CASCADE
		)
	`)
	if err != nil {
		return fmt.Errorf("failed to create subtitles table: %w", err)
	}

	return nil
}

// GetVideoByURL finds a video by a URL pattern containing the video ID
func (r *Repository) GetVideoByURL(ctx context.Context, videoID string) (*Video, error) {
	var video Video
	found, err := r.db.From("videos").
		Select("id", "original_url", "title").
		Where(goqu.L("original_url LIKE ?", "%"+videoID+"%")).
		ScanStructContext(ctx, &video)

	if err != nil {
		return nil, fmt.Errorf("failed to query video: %w", err)
	}
	if !found {
		return nil, sql.ErrNoRows
	}

	return &video, nil
}

// GetSubtitlesByVideoID retrieves all subtitles for a given video ID
func (r *Repository) GetSubtitlesByVideoID(ctx context.Context, videoID int) ([]Subtitle, error) {
	var subtitles []Subtitle
	err := r.db.From("subtitles").
		Select("id", "video_id", "language", "type", "content").
		Where(goqu.C("video_id").Eq(videoID)).
		ScanStructsContext(ctx, &subtitles)

	if err != nil {
		return nil, fmt.Errorf("failed to query subtitles: %w", err)
	}

	if subtitles == nil {
		subtitles = []Subtitle{}
	}

	return subtitles, nil
}

// ListAllVideos retrieves all videos with their subtitles
func (r *Repository) ListAllVideos(ctx context.Context) ([]VideoWithSubs, error) {
	// First get all videos
	var videos []Video
	err := r.db.From("videos").
		Select("id", "original_url", "title").
		ScanStructsContext(ctx, &videos)

	if err != nil {
		return nil, fmt.Errorf("failed to query videos: %w", err)
	}

	if videos == nil {
		return []VideoWithSubs{}, nil
	}

	// For each video, get its subtitles
	result := make([]VideoWithSubs, 0, len(videos))
	for _, video := range videos {
		var subtitles []Subtitle
		err := r.db.From("subtitles").
			Select("id", "video_id", "language", "type").
			Where(goqu.C("video_id").Eq(video.ID)).
			ScanStructsContext(ctx, &subtitles)

		if err != nil {
			slog.Warn("Failed to get subtitles for video",
				"video_id", video.ID,
				"error", err)
			subtitles = []Subtitle{}
		}

		if subtitles == nil {
			subtitles = []Subtitle{}
		}

		result = append(result, VideoWithSubs{
			Video:     video,
			Subtitles: subtitles,
		})
	}

	return result, nil
}

// CreateVideo inserts a new video and returns its ID
func (r *Repository) CreateVideo(ctx context.Context, url, title string) (int64, error) {
	result, err := r.db.Insert("videos").
		Rows(goqu.Record{"original_url": url, "title": title}).
		Executor().
		ExecContext(ctx)

	if err != nil {
		return 0, fmt.Errorf("failed to insert video: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("failed to get last insert id: %w", err)
	}

	return id, nil
}

// DeleteVideo removes a video by ID
func (r *Repository) DeleteVideo(ctx context.Context, id int) error {
	_, err := r.db.Delete("videos").
		Where(goqu.C("id").Eq(id)).
		Executor().
		ExecContext(ctx)

	if err != nil {
		return fmt.Errorf("failed to delete video: %w", err)
	}

	return nil
}

// CreateSubtitle inserts a new subtitle
func (r *Repository) CreateSubtitle(ctx context.Context, videoID int, language, subType, content string) error {
	_, err := r.db.Insert("subtitles").
		Rows(goqu.Record{
			"video_id": videoID,
			"language": language,
			"type":     subType,
			"content":  content,
		}).
		Executor().
		ExecContext(ctx)

	if err != nil {
		return fmt.Errorf("failed to insert subtitle: %w", err)
	}

	return nil
}

// DeleteSubtitle removes a subtitle by ID
func (r *Repository) DeleteSubtitle(ctx context.Context, id int) error {
	_, err := r.db.Delete("subtitles").
		Where(goqu.C("id").Eq(id)).
		Executor().
		ExecContext(ctx)

	if err != nil {
		return fmt.Errorf("failed to delete subtitle: %w", err)
	}

	return nil
}
