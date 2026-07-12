package payloads

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpen_FileFallback_SingleInstance(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "payloads")
	t.Setenv("PGHOST", "")
	t.Setenv("VERSIOND_FORCE", "v2")
	t.Setenv("DEVSHARD_REQUIRE_POSTGRES", "")

	store, closeFn, err := Open(context.Background(), OpenConfig{Dir: dir})
	require.NoError(t, err)
	defer closeFn()

	_, ok := store.(*FileStorage)
	require.True(t, ok, "expected file storage when PGHOST unset and single VERSIOND_FORCE")

	ctx := context.Background()
	prompt := []byte(`{"prompt":true}`)
	response := []byte(`{"response":true}`)
	require.NoError(t, store.Store(ctx, "escrow-1", 1, 10, prompt, response))

	gotPrompt, gotResponse, err := store.Retrieve(ctx, "escrow-1", 1, 10)
	require.NoError(t, err)
	assert.Equal(t, prompt, gotPrompt)
	assert.Equal(t, response, gotResponse)
}

func TestOpen_RequiresPostgres_MultiVersionForce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "payloads")
	t.Setenv("PGHOST", "")
	t.Setenv("VERSIOND_FORCE", "v1,v2")
	t.Setenv("DEVSHARD_REQUIRE_POSTGRES", "")

	_, _, err := Open(context.Background(), OpenConfig{Dir: dir})
	require.ErrorIs(t, err, ErrSharedPostgresRequired)
}

func TestOpen_RequiresPostgres_ExplicitFlag(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "payloads")
	t.Setenv("PGHOST", "")
	t.Setenv("VERSIOND_FORCE", "v2")
	t.Setenv("DEVSHARD_REQUIRE_POSTGRES", "1")

	_, _, err := Open(context.Background(), OpenConfig{Dir: dir})
	require.ErrorIs(t, err, ErrSharedPostgresRequired)
}

func TestFileStorage_DropEpoch(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStorage(dir)
	ctx := context.Background()

	require.NoError(t, store.Store(ctx, "escrow-1", 1, 9, []byte("a"), []byte("b")))
	require.NoError(t, store.Store(ctx, "escrow-1", 2, 10, []byte("c"), []byte("d")))

	require.NoError(t, store.DropEpoch(ctx, 10))

	_, _, err := store.Retrieve(ctx, "escrow-1", 1, 9)
	require.NoError(t, err)
	_, _, err = store.Retrieve(ctx, "escrow-1", 2, 10)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestRequiresSharedPostgres(t *testing.T) {
	t.Setenv("DEVSHARD_REQUIRE_POSTGRES", "")
	t.Setenv("VERSIOND_FORCE", "")
	assert.False(t, requiresSharedPostgres())

	t.Setenv("VERSIOND_FORCE", "v2")
	assert.False(t, requiresSharedPostgres())

	t.Setenv("VERSIOND_FORCE", "v1, v2")
	assert.True(t, requiresSharedPostgres())

	t.Setenv("VERSIOND_FORCE", "v2")
	t.Setenv("DEVSHARD_REQUIRE_POSTGRES", "true")
	assert.True(t, requiresSharedPostgres())
}
