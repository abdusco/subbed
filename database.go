package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/doug-martin/goqu/v9"
	_ "github.com/doug-martin/goqu/v9/dialect/sqlite3"
	_ "modernc.org/sqlite"

	"subbed/internal/logger"
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
	// foreign_keys and busy_timeout apply to one connection only, so they are set for every connection of the
	// pool here, not with PRAGMA statements below. Without foreign_keys, deleting a job or video could leave its
	// logs, jobs and subtitles behind.
	dsn := "file:" + strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(dbPath) +
		"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Set good SQLite pragmas for performance and reliability
	pragmas := []string{
		"PRAGMA journal_mode=WAL",            // Write-Ahead Logging for better concurrency
		"PRAGMA synchronous=NORMAL",          // Balanced durability/performance
		"PRAGMA cache_size=-64000",           // 64MB cache
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

// schema creates every table. There are no migrations: while subbed is a prototype, a changed table
// means starting from a fresh database.
const schema = `
CREATE TABLE IF NOT EXISTS videos (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	original_url TEXT NOT NULL UNIQUE,
	title TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS subtitles (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	video_id INTEGER NOT NULL,
	language TEXT NOT NULL,
	type TEXT NOT NULL,
	content TEXT NOT NULL,
	UNIQUE (video_id, language),
	FOREIGN KEY (video_id) REFERENCES videos(id) ON DELETE CASCADE
);

-- browser cookies for yt-dlp
CREATE TABLE IF NOT EXISTS cookies (
	domain TEXT NOT NULL,
	key TEXT NOT NULL,
	value TEXT NOT NULL,
	path TEXT NOT NULL DEFAULT '/',
	secure INTEGER NOT NULL DEFAULT 0,
	http_only INTEGER NOT NULL DEFAULT 0,
	expires INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (domain, key)
);

-- transcribe and translate jobs, and what they logged
CREATE TABLE IF NOT EXISTS jobs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	video_id INTEGER NOT NULL,
	kind TEXT NOT NULL,
	status TEXT NOT NULL,
	progress INTEGER NOT NULL DEFAULT 0,
	stage TEXT NOT NULL DEFAULT '',
	languages TEXT NOT NULL,
	source_language TEXT NOT NULL DEFAULT '',
	error TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	FOREIGN KEY (video_id) REFERENCES videos(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS job_logs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	job_id INTEGER NOT NULL,
	at INTEGER NOT NULL,
	level TEXT NOT NULL,
	message TEXT NOT NULL,
	FOREIGN KEY (job_id) REFERENCES jobs(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS job_logs_job ON job_logs (job_id, id);

-- settings that the admin can change
CREATE TABLE IF NOT EXISTS settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

// initDB creates the database tables if they don't exist
func (r *Repository) initDB() error {
	sqlDB, ok := r.db.Db.(*sql.DB)
	if !ok {
		return fmt.Errorf("failed to get sql.DB instance")
	}
	if _, err := sqlDB.Exec(schema); err != nil {
		return fmt.Errorf("failed to create tables: %w", err)
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

// GetSubtitle returns a video's subtitle in one language, or sql.ErrNoRows.
func (r *Repository) GetSubtitle(ctx context.Context, videoID int, language string) (*Subtitle, error) {
	var subtitle Subtitle
	found, err := r.db.From("subtitles").
		Select("id", "video_id", "language", "type", "content").
		Where(goqu.C("video_id").Eq(videoID), goqu.C("language").Eq(language)).
		ScanStructContext(ctx, &subtitle)
	if err != nil {
		return nil, fmt.Errorf("failed to query subtitle: %w", err)
	}
	if !found {
		return nil, sql.ErrNoRows
	}
	return &subtitle, nil
}

// GetSubtitlesByVideoID retrieves all subtitles for a given video ID
func (r *Repository) GetSubtitlesByVideoID(ctx context.Context, videoID int) ([]Subtitle, error) {
	var subtitles []Subtitle
	err := r.db.From("subtitles").
		Select("id", "video_id", "language", "type", "content").
		Where(goqu.C("video_id").Eq(videoID)).
		Order(goqu.C("id").Asc()).
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
		Order(goqu.C("id").Desc()).
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
			Order(goqu.C("id").Asc()).
			ScanStructsContext(ctx, &subtitles)

		if err != nil {
			logger.FromCtx(ctx).Warn("Failed to get subtitles for video",
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

// UpdateVideoTitle changes a video's title and reports whether the video exists
func (r *Repository) UpdateVideoTitle(ctx context.Context, id int, title string) (bool, error) {
	result, err := r.db.Update("videos").
		Set(goqu.Record{"title": title}).
		Where(goqu.C("id").Eq(id)).
		Executor().
		ExecContext(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to update video: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("failed to read update result: %w", err)
	}

	return affected > 0, nil
}

// SaveSubtitle stores a subtitle, replacing the video's existing one in the same language
func (r *Repository) SaveSubtitle(ctx context.Context, videoID int, language, subType, content string) error {
	_, err := r.db.Insert("subtitles").
		Rows(goqu.Record{
			"video_id": videoID,
			"language": language,
			"type":     subType,
			"content":  content,
		}).
		OnConflict(goqu.DoUpdate("video_id, language", goqu.Record{
			"type":    goqu.L("excluded.type"),
			"content": goqu.L("excluded.content"),
		})).
		Executor().
		ExecContext(ctx)

	if err != nil {
		return fmt.Errorf("failed to save subtitle: %w", err)
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
