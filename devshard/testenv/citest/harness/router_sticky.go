package harness

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"devshard/testenv/config"

	"github.com/stretchr/testify/require"
)

// StickyUpstreamHeader is nginx $upstream_addr exposed by versiond-router.
const StickyUpstreamHeader = "X-Upstream-Addr"

// FindDistinctStickySessions returns two session ids routed to different versiond upstreams.
func FindDistinctStickySessions(t *testing.T, client *http.Client, routerHTTP, version string) (sessionA, upstreamA, sessionB, upstreamB string) {
	t.Helper()
	if client == nil {
		client = HTTPClient()
	}
	sessionA = "citest-s6-session-a"
	urlA := RouterSessionURL(routerHTTP, version, sessionA, "/healthz")
	upstreamA = RequireResponseHeader(t, client, urlA, StickyUpstreamHeader)

	for n := 0; n < 64; n++ {
		candidate := fmt.Sprintf("citest-s6-%d", n)
		if candidate == sessionA {
			continue
		}
		urlB := RouterSessionURL(routerHTTP, version, candidate, "/healthz")
		got, err := GetResponseHeader(client, urlB, StickyUpstreamHeader)
		if err != nil {
			t.Fatalf("probe session %q: %v", candidate, err)
		}
		if got != "" && got != upstreamA {
			return sessionA, upstreamA, candidate, got
		}
	}
	t.Fatalf("could not find a second sticky upstream distinct from %q", upstreamA)
	return "", "", "", ""
}

// HostIDForUpstream maps nginx upstream addr (host:port) to compose service id.
func HostIDForUpstream(cfg *config.File, upstreamAddr string) string {
	if cfg == nil {
		return ""
	}
	host := strings.TrimSpace(upstreamAddr)
	if i := strings.Index(host, ":"); i >= 0 {
		host = host[:i]
	}
	for _, h := range cfg.Hosts {
		if h.IP == host {
			return h.ID
		}
	}
	return ""
}

// RequireGETStatusIn asserts GET returns one of the allowed HTTP status codes.
func RequireGETStatusIn(t *testing.T, client *http.Client, url string, allowed ...int) int {
	t.Helper()
	if client == nil {
		client = HTTPClient()
	}
	resp, err := client.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	for _, code := range allowed {
		if resp.StatusCode == code {
			return resp.StatusCode
		}
	}
	t.Fatalf("GET %s: status %d not in %v", url, resp.StatusCode, allowed)
	return resp.StatusCode
}

// RequireGETNotGatewayError asserts the router still reaches a live versiond upstream.
func RequireGETNotGatewayError(t *testing.T, client *http.Client, url, wantUpstream string) {
	t.Helper()
	if client == nil {
		client = HTTPClient()
	}
	resp, err := client.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.NotEqual(t, http.StatusBadGateway, resp.StatusCode,
		"expected live upstream, got 502 from %s", url)
	require.NotEqual(t, http.StatusServiceUnavailable, resp.StatusCode,
		"expected live upstream, got 503 from %s", url)
	got := resp.Header.Get(StickyUpstreamHeader)
	require.Equal(t, wantUpstream, got, "sticky upstream changed for %s", url)
}

// FaultRouteOutcome is how versiond-router behaves after a versiond instance stops.
type FaultRouteOutcome int

const (
	FaultRouteFailed FaultRouteOutcome = iota // 502/503 — sticky upstream unavailable
	FaultRouteRerouted                          // consistent-hash ring shrank; session hit survivor
)

// WaitStoppedUpstreamOutcome polls until a session pinned to a stopped versiond either
// fails (502/503) or is re-hashed to the surviving upstream when Docker DNS drops the peer.
func WaitStoppedUpstreamOutcome(t *testing.T, client *http.Client, url, stoppedUpstream, survivorUpstream string, wait time.Duration) FaultRouteOutcome {
	t.Helper()
	if client == nil {
		client = HTTPClient()
	}
	var outcome FaultRouteOutcome
	ok := AssertEventually(t, wait, time.Second, func() bool {
		resp, err := client.Get(url)
		if err != nil {
			outcome = FaultRouteFailed
			return true
		}
		defer resp.Body.Close()
		upstream := resp.Header.Get(StickyUpstreamHeader)
		switch {
		case resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable:
			outcome = FaultRouteFailed
			return true
		case upstream == survivorUpstream && upstream != stoppedUpstream:
			outcome = FaultRouteRerouted
			return true
		default:
			return false
		}
	})
	require.True(t, ok,
		"session on stopped upstream %s did not fail or reroute to %s within %s",
		stoppedUpstream, survivorUpstream, wait)
	return outcome
}
