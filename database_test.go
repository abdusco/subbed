package main

import (
	"context"
	"path/filepath"
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
