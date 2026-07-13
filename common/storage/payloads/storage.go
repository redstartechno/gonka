package payloads

import (
	"context"
	"errors"
)

// ErrNotFound is returned when a requested payload does not exist.
var ErrNotFound = errors.New("payloads: not found")

// ErrSharedPostgresRequired is returned when HA mode is enabled but PGHOST is unset.
var ErrSharedPostgresRequired = errors.New(
	"payloads: shared Postgres required in HA mode (set PGHOST and PG* env, or unset DEVSHARD_HA / DEVSHARD_REQUIRE_POSTGRES)",
)

// Storage persists inference prompt/response bytes keyed by escrow, inference, and epoch.
type Storage interface {
	Store(ctx context.Context, escrowId string, inferenceId, epochId uint64, prompt, response []byte) error
	Retrieve(ctx context.Context, escrowId string, inferenceId, epochId uint64) (prompt, response []byte, err error)
	DropEpoch(ctx context.Context, epochId uint64) error
}
