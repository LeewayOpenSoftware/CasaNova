package scheduled

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// maxCapturedOutput bounds the stdout+stderr we keep per run. Enough
// to see what happened in a normal task, small enough that a chatty
// process can't fill the task store.
const maxCapturedOutput = 64 * 1024 // 64 KiB

// Run executes a task once and returns a completed Run record.
// Honors the task's timeout by killing the child process when it
// expires. Errors from the runner itself (cannot start, docker exec
// not found, etc.) become Status=StatusError records — the run still
// appears in history so the user isn't left wondering why nothing
// happened.
func RunTask(ctx context.Context, t *Task) Run {
	run := Run{
		ID:        newRunID(),
		TaskID:    t.ID,
		StartedAt: time.Now(),
	}
	defer func() {
		run.EndedAt = time.Now()
	}()

	// Default to 24 hours if no timeout is set to prevent infinite runs.
	// Wave 26: don't mutate the stored task — Run() used to write
	// 86400 back into t.TimeoutSeconds, polluting the in-memory store
	// (and the next saveLocked) with a value the user never set.
	timeoutSeconds := t.TimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = 86400
	}

	// Apply per-task timeout on top of the caller's ctx.
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	cmd, err := buildCmd(ctx, t)
	if err != nil {
		run.Status = StatusError
		run.Output = err.Error()
		return run
	}

	var buf bytes.Buffer
	// Cap the output buffer up-front so a chatty process can't OOM us.
	// We use a limitedBuffer wrapper that discards past the cap but
	// preserves the head of the output (first N bytes), which is
	// usually where the diagnostic signal is.
	lb := &limitedBuffer{Buffer: &buf, limit: maxCapturedOutput}
	cmd.Stdout = lb
	cmd.Stderr = lb

	err = cmd.Run()
	run.Output = buf.String()
	if err == nil {
		run.Status = StatusSuccess
		run.ExitCode = 0
		return run
	}

	// Distinguish timeout from regular failure so the UI can show
	// "timed out" vs "failed" differently.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		run.Status = StatusTimedOut
		if exit, ok := err.(*exec.ExitError); ok {
			run.ExitCode = exit.ExitCode()
		} else {
			run.ExitCode = -1
		}
		return run
	}

	run.Status = StatusFailed
	if exit, ok := err.(*exec.ExitError); ok {
		run.ExitCode = exit.ExitCode()
	} else {
		// Runner failed to even spawn (e.g. docker not installed).
		run.Status = StatusError
		run.ExitCode = -1
		if run.Output == "" {
			run.Output = err.Error()
		}
	}
	return run
}

// buildCmd constructs the exec.Cmd for a task, routing through
// `docker exec` when the target is a container. Using separate argv
// (not sh -c "...") at the outer level keeps the boundary between
// runner config and user command clear; user-supplied shell is still
// invoked as the last argument via sh -c on the inner side.
//
// Wave 26: Validate() is the primary metachar / workdir gate at PUT
// time. The same checks are repeated here as defense in depth — if a
// future code path constructs a Task struct directly (tests, restore
// from backup, etc.) and skips Validate, the runner still refuses to
// exec a hostile command.
func buildCmd(ctx context.Context, t *Task) (*exec.Cmd, error) {
	if shellMetaRe.MatchString(t.Command) {
		return nil, fmt.Errorf("task %s: command contains disallowed shell metacharacters", t.ID)
	}
	// Validate WorkingDir to prevent path traversal attacks.
	if t.Target.WorkingDir != "" {
		if shellMetaRe.MatchString(t.Target.WorkingDir) {
			return nil, fmt.Errorf("task %s: workdir contains disallowed characters", t.ID)
		}
		cleaned := filepath.Clean(t.Target.WorkingDir)
		if strings.Contains(cleaned, "..") {
			return nil, fmt.Errorf("task %s: workdir must not contain path traversal", t.ID)
		}
	}
	if t.Target.Host {
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", t.Command)
		if t.Target.WorkingDir != "" {
			cmd.Dir = filepath.Clean(t.Target.WorkingDir)
		}
		return cmd, nil
	}
	if t.Target.Container != "" {
		if !containerNameRe.MatchString(t.Target.Container) {
			return nil, fmt.Errorf("task %s: invalid container name %q", t.ID, t.Target.Container)
		}
		// Wave 34-X residual: harden the docker exec invocation.
		//
		//   --interactive=false / --tty=false (the defaults, but we set
		//     them explicitly to defeat any future docker-cli default
		//     change). A TTY-allocated exec on a hung container can
		//     keep this goroutine pinned forever; an interactive stdin
		//     pipe gives a hostile entrypoint a feedback channel.
		//   --user, when supplied by the operator, runs the command as
		//     a non-root account inside the container. Without this,
		//     every scheduled task ran as the container's default user
		//     (almost always root in stock images), and any task that
		//     ended up in a container with /var/run/docker.sock mounted
		//     was a one-line escape to host root.
		args := []string{"exec", "--interactive=false", "--tty=false"}
		if t.Target.WorkingDir != "" {
			args = append(args, "-w", filepath.Clean(t.Target.WorkingDir))
		}
		if t.Target.User != "" {
			if !containerUserRe.MatchString(t.Target.User) {
				return nil, fmt.Errorf("task %s: invalid container user %q", t.ID, t.Target.User)
			}
			args = append(args, "--user", t.Target.User)
		}
		args = append(args, t.Target.Container, "/bin/sh", "-c", t.Command)
		return exec.CommandContext(ctx, "docker", args...), nil
	}
	return nil, fmt.Errorf("task %s has no valid target", t.ID)
}

// limitedBuffer writes only up to limit bytes into the underlying
// Buffer; further writes are reported as successful to the caller
// but silently dropped. This lets us bound memory without breaking
// the caller's error handling for a write failure.
type limitedBuffer struct {
	Buffer *bytes.Buffer
	limit  int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	remaining := l.limit - l.Buffer.Len()
	if remaining <= 0 {
		return len(p), nil // drop, pretend success
	}
	if len(p) > remaining {
		l.Buffer.Write(p[:remaining])
		l.Buffer.WriteString("\n... (output truncated)\n")
		return len(p), nil
	}
	return l.Buffer.Write(p)
}

// newRunID generates a short identifier for a run record.
func newRunID() string {
	return fmt.Sprintf("run_%x", time.Now().UnixNano())
}
