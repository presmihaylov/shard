package sandbox

import (
	"context"

	"github.com/presmihaylov/shard/models"
)

type forkCheckKey struct{}

// WithForkCheck lets a fork authorize its grants under the source lock, avoiding an earlier inspection.
func WithForkCheck(ctx context.Context, check func(models.Sandbox) error) context.Context {
	return context.WithValue(ctx, forkCheckKey{}, check)
}

func checkFork(ctx context.Context, source models.Sandbox) error {
	check, _ := ctx.Value(forkCheckKey{}).(func(models.Sandbox) error)
	if check == nil {
		return nil
	}

	return check(source)
}
