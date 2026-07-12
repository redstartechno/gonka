package queryapitest

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// canonicalReadOnlyRoutes is the full set of read-only /v1/ query paths owned by
// edge-api (must match proxy/entrypoint.sh EDGE_API_ROUTE_PATHS_DEFAULT and openapi.yaml).
var canonicalReadOnlyRoutes = []string{
	"/v1/status",
	"/v1/versions",
	"/v1/models",
	"/v1/governance/models",
	"/v1/governance/models-legacy",
	"/v1/pricing",
	"/v1/participants",
	"/v1/participants/{address}",
	"/v1/epochs/{epoch}",
	"/v1/epochs/{epoch}/participants",
	"/v1/poc-batches/{epoch}",
	"/v1/restrictions/status",
	"/v1/restrictions/exemptions",
	"/v1/restrictions/exemptions/{id}/usage/{account}",
	"/v1/bls/epoch/{id}",
	"/v1/bls/epochs/{id}",
	"/v1/bls/signatures/{request_id}",
	"/v1/bridge/addresses",
	"/v1/verify-proof",
	"/v1/verify-block",
	"/v1/debug/pubkey-to-addr/{pubkey}",
	"/v1/debug/verify/{height}",
}

func TestReadOnlyRouteCount(t *testing.T) {
	assert.Len(t, canonicalReadOnlyRoutes, 22)
}

func TestOpenAPIPathsMatchCanonicalReadOnlyRoutes(t *testing.T) {
	got := loadOpenAPIPaths(t)
	sort.Strings(got)
	want := append([]string(nil), canonicalReadOnlyRoutes...)
	sort.Strings(want)
	assert.Equal(t, want, got)
}

func TestProxyEntrypointRoutesMatchCanonicalReadOnlyRoutes(t *testing.T) {
	got := loadProxyEdgeAPIRoutePaths(t)
	sort.Strings(got)
	want := append([]string(nil), canonicalReadOnlyRoutes...)
	sort.Strings(want)
	assert.Equal(t, want, got)
}

func loadOpenAPIPaths(t *testing.T) []string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	path := filepath.Join(filepath.Dir(file), "..", "openapi.yaml")
	b, err := os.ReadFile(path)
	require.NoError(t, err)

	re := regexp.MustCompile(`(?m)^  (/v1/[^:]+):`)
	matches := re.FindAllStringSubmatch(string(b), -1)
	require.NotEmpty(t, matches)

	paths := make([]string, 0, len(matches))
	for _, m := range matches {
		paths = append(paths, m[1])
	}
	return paths
}

func loadProxyEdgeAPIRoutePaths(t *testing.T) []string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", ".."))
	path := filepath.Join(repoRoot, "proxy", "entrypoint.sh")
	b, err := os.ReadFile(path)
	require.NoError(t, err)

	re := regexp.MustCompile(`(?m)^(/v1/[^\s']+)`)
	section := extractEdgeAPIRoutePathsSection(string(b))
	matches := re.FindAllStringSubmatch(section, -1)
	require.NotEmpty(t, matches, "EDGE_API_ROUTE_PATHS_DEFAULT not found in proxy/entrypoint.sh")

	paths := make([]string, 0, len(matches))
	for _, m := range matches {
		paths = append(paths, strings.TrimSpace(m[1]))
	}
	return paths
}

func extractEdgeAPIRoutePathsSection(script string) string {
	const start = "EDGE_API_ROUTE_PATHS_DEFAULT='"
	const end = "'"
	i := strings.Index(script, start)
	if i < 0 {
		return ""
	}
	rest := script[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		return ""
	}
	return rest[:j]
}
