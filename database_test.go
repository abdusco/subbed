package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestRepository(t *testing.T) *Repository {
	t.Helper()
	repo, err := NewRepository(filepath.Join(t.TempDir(), "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { repo.Close() })
	return repo
}

func TestEveryConnectionGetsThePragmas(t *testing.T) {
	ctx := context.Background()
	// a path with characters that would end the path of a URL
	path := filepath.Join(t.TempDir(), "a#b?c%d.db")
	repo, err := NewRepository(path)
	require.NoError(t, err)
	assert.FileExists(t, path, "the database is at the path as given")
	t.Cleanup(func() { repo.Close() })
	pool := repo.db.Db.(*sql.DB)

	// holding the connections at once makes the pool open a new one for each
	var conns []*sql.Conn
	for range 3 {
		conn, err := pool.Conn(ctx)
		require.NoError(t, err)
		defer conn.Close()
		conns = append(conns, conn)
	}
	for _, conn := range conns {
		var foreignKeys, busyTimeout int
		require.NoError(t, conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys))
		require.NoError(t, conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout))
		assert.Equal(t, 1, foreignKeys)
		assert.Equal(t, 5000, busyTimeout)
	}

	var mode string
	require.NoError(t, pool.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode))
	assert.Equal(t, "wal", mode)
	var autoVacuum int
	require.NoError(t, pool.QueryRowContext(ctx, "PRAGMA auto_vacuum").Scan(&autoVacuum))
	assert.Equal(t, 2, autoVacuum, "incremental, which only works when set before the first write")
}

func TestSQLiteDSNKeepsRelativePathsRelative(t *testing.T) {
	assert.True(t, strings.HasPrefix(sqliteDSN("./subbed.db"), "file:./subbed.db?_pragma="), sqliteDSN("./subbed.db"))
	assert.True(t, strings.HasPrefix(sqliteDSN("subbed.db"), "file:subbed.db?_pragma="), sqliteDSN("subbed.db"))
}

func TestSaveSubtitleReplacesSameLanguage(t *testing.T) {
	ctx := context.Background()
	repo := newTestRepository(t)
	videoID, err := repo.CreateVideo(ctx, "https://youtu.be/jNQXAC9IVRw", "Me at the zoo")
	require.NoError(t, err)

	for _, save := range []struct{ language, content string }{{"en", "first"}, {"tr", "merhaba"}, {"en", "second"}} {
		require.NoError(t, repo.SaveSubtitle(ctx, int(videoID), save.language, "srt", save.content))
	}

	subtitles, err := repo.GetSubtitlesByVideoID(ctx, int(videoID))
	require.NoError(t, err)
	got := map[string]string{}
	for _, subtitle := range subtitles {
		got[subtitle.Language] = subtitle.Content
	}
	assert.Equal(t, map[string]string{"en": "second", "tr": "merhaba"}, got)
	assert.Len(t, subtitles, 2)
}

func TestOpeningTheDatabaseAgainKeepsItsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "again.db")
	repo, err := NewRepository(path)
	require.NoError(t, err)
	_, err = repo.CreateVideo(context.Background(), "https://youtu.be/jNQXAC9IVRw", "Me at the zoo")
	require.NoError(t, err)
	require.NoError(t, repo.Close())

	again, err := NewRepository(path)
	require.NoError(t, err)
	defer again.Close()
	videos, err := again.ListAllVideos(context.Background())
	require.NoError(t, err)
	require.Len(t, videos, 1)
	assert.Equal(t, "Me at the zoo", videos[0].Title)
}
