//go:build testenvci

package citest

import (
	"testing"
	"time"

	"devshard/testenv/citest/harness"

	"github.com/stretchr/testify/require"
)

// TestS6_VersiondStop verifies versiond-router consistent-hash does not failover a
// sticky session to another versiond when its upstream is stopped; sessions hashed to
// a live upstream keep working.
func TestS6_VersiondStop(t *testing.T) {
	harness.SkipUnlessEnv(t, "TESTENV_CITEST")
	harness.RequireDocker(t)

	stack, cfg, eps := harness.BootS1Stack(t, "citest-s6-*")
	client := harness.HTTPClient()
	t.Cleanup(func() {
		if t.Failed() {
			harness.DumpComposeLogs(t, stack, "versiond-0", "versiond-1", "versiond-router")
		}
	})

	harness.WaitGETOK(t, client, eps.RouterHTTP+"/healthz", 5*time.Minute, "versiond-router healthz", stack)

	version := cfg.Versiond.VersionName
	sessionA, upstreamA, sessionB, upstreamB := harness.FindDistinctStickySessions(t, client, eps.RouterHTTP, version)
	require.NotEqual(t, upstreamA, upstreamB)

	stoppedHost := harness.HostIDForUpstream(cfg, upstreamA)
	require.NotEmpty(t, stoppedHost, "map upstream %q to host id", upstreamA)
	harness.Step(t, "session %q → %s; session %q → %s; stopping %s",
		sessionA, upstreamA, sessionB, upstreamB, stoppedHost)

	stack.StopService(t, stoppedHost)

	urlA := harness.RouterSessionURL(eps.RouterHTTP, version, sessionA, "/healthz")
	urlB := harness.RouterSessionURL(eps.RouterHTTP, version, sessionB, "/healthz")

	// Sticky hash pins the session to one upstream; when that versiond stops, nginx either
	// returns 502/503 (peer unavailable) or re-hashes to the surviving instance (ring shrink).
	harness.Step(t, "session on stopped upstream should fail or reroute (no silent stickiness to dead peer)")
	outcome := harness.WaitStoppedUpstreamOutcome(t, client, urlA, upstreamA, upstreamB, 45*time.Second)
	switch outcome {
	case harness.FaultRouteFailed:
		harness.Step(t, "observed gateway error for session on stopped %s", stoppedHost)
	case harness.FaultRouteRerouted:
		harness.Step(t, "observed consistent-hash reroute to surviving upstream %s", upstreamB)
	}

	// Surviving upstream still serves requests for sessions hashed to it.
	harness.Step(t, "session on live upstream should still route to %s", upstreamB)
	harness.RequireGETNotGatewayError(t, client, urlB, upstreamB)
}
