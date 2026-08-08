// Package execx: ejecucion de comandos externos con timeout. Todo el panel
// habla con el sistema (tmux, git, gh, ss, ps...) a traves de estas dos
// funciones para que ningun subproceso pueda colgar el render.
package execx

import (
	"context"
	"os/exec"
	"time"
)

func Run(ctx context.Context, dir string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func RunTimeout(d time.Duration, dir string, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return Run(ctx, dir, name, args...)
}
