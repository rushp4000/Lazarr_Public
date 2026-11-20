// export_test.go exposes internal constructors for the test package only.
// It is compiled exclusively as part of the test binary.
package torbox

import (
	"context"
	"time"

	"github.com/your-username/lazarr/internal/config"
)

// NewForTest constructs a *client with the base URL taken from cfg.APIBase so
// tests can point at an httptest.Server without touching production code paths.
func NewForTest(cfg config.TorBox, opts ...option) Client {
	return newClient(cfg, opts...)
}

// WithClock replaces the governor's clock and sleep so rate-limit tests are deterministic.
func WithClock(now func() time.Time, sleep func(context.Context, time.Duration) error) option {
	return withClock(now, sleep)
}

// BackoffUntilForTest reports the client's armed API backoff.
func BackoffUntilForTest(c Client) (time.Time, bool) { return c.(*client).BackoffUntil() }
