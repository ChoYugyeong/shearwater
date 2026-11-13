package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/errdefs"
)

// ProbeExec uses the container's process environment/user/cwd. It does not run
// a host shell and never turns an argv list into a shell command. Output is
// discarded by the shim, so a noisy health command cannot fill node memory.
func (r *Containerd) ProbeExec(ctx context.Context, id string, command []string) (result error) {
	client, err := r.conn()
	if err != nil {
		return err
	}
	ctx = inNS(ctx)
	ct, err := client.LoadContainer(ctx, id)
	if err != nil {
		return err
	}
	spec, err := ct.Spec(ctx)
	if err != nil {
		return err
	}
	if spec.Process == nil {
		return fmt.Errorf("container has no process specification")
	}
	task, err := ct.Task(ctx, nil)
	if err != nil {
		return err
	}
	process := *spec.Process
	process.Args = append([]string(nil), command...)
	process.Terminal = false
	nonce := make([]byte, 12)
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	exec, err := task.Exec(ctx, "probe-"+hex.EncodeToString(nonce), &process, cio.NullIO)
	if err != nil {
		return err
	}
	defer func() {
		clean, cancel := context.WithTimeout(inNS(context.Background()), 5*time.Second)
		defer cancel()
		// A signal is asynchronous. WithProcessKill waits for the exec process
		// to stop before deleting its shim bookkeeping. Deleting immediately
		// after SIGKILL can race the exit and leave failed probes accumulating.
		if _, err := exec.Delete(clean, containerd.WithProcessKill); err != nil && !errdefs.IsNotFound(err) {
			result = errors.Join(result, fmt.Errorf("clean up probe process: %w", err))
		}
	}()
	exit, err := exec.Wait(ctx)
	if err != nil {
		return err
	}
	if err = exec.Start(ctx); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case status := <-exit:
		code, _, err := status.Result()
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("exec exited with code %d", code)
		}
		return nil
	}
}
