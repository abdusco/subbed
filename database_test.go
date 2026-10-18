package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func newTestRepository(t *testing.T) *Repository {
	t.Helper()
	repo, err := NewRepository(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { repo.Close() })
	return repo
}

func TestSaveSubtitleReplacesSameLanguage(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepository(t)
	videoID, err := repo.CreateVideo(ctx, "https://youtu.be/jNQXAC9IVRw", "Me at the zoo")
	if err != nil {
		t.Fatal(err)
	}

	for _, save := range []struct{ language, content string }{{"en", "first"}, {"tr", "merhaba"}, {"en", "second"}} {
		if err := repo.SaveSubtitle(ctx, int(videoID), save.language, "srt", save.content); err != nil {
			t.Fatal(err)
		}
	}

	subtitles, err := repo.GetSubtitlesByVideoID(ctx, int(videoID))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, subtitle := range subtitles {
		got[subtitle.Language] = subtitle.Content
	}
	if len(subtitles) != 2 || got["en"] != "second" || got["tr"] != "merhaba" {
		t.Fatalf("subtitles = %+v", subtitles)
	}
}

func TestOpeningADatabaseKeepsTheNewestDuplicateSubtitle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE videos (id INTEGER PRIMARY KEY AUTOINCREMENT, original_url TEXT NOT NULL UNIQUE, title TEXT NOT NULL)`,
		`CREATE TABLE subtitles (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			video_id INTEGER NOT NULL,
			language TEXT NOT NULL,
			type TEXT NOT NULL,
			content TEXT NOT NULL,
			FOREIGN KEY (video_id) REFERENCES videos(id) ON DELETE CASCADE
		)`,
		`INSERT INTO videos (original_url, title) VALUES ('https://youtu.be/jNQXAC9IVRw', 'Me at the zoo')`,
		`INSERT INTO subtitles (video_id, language, type, content) VALUES (1, 'en', 'srt', 'old'), (1, 'en', 'srt', 'newest'), (1, 'tr', 'srt', 'merhaba')`,
	} {
		if _, err := old.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	old.Close()

	repo, err := NewRepository(path)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()

	subtitles, err := repo.GetSubtitlesByVideoID(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, subtitle := range subtitles {
		got[subtitle.Language] = subtitle.Content
	}
	if len(subtitles) != 2 || got["en"] != "newest" || got["tr"] != "merhaba" {
		t.Fatalf("subtitles = %+v", subtitles)
	}
}
