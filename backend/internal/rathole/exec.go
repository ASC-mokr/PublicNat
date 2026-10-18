package rathole

import (
	"context"
	"os/exec"
)

// execCommandContext — тонкая обёртка над os/exec (вынесена для тестируемости).
func execCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}
