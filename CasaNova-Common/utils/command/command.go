package command

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	exec2 "github.com/LeewayOpenSoftware/CasaNova-Common/utils/exec"
)

// defaultExecCeiling bounds a shell command that has no opinion of its own.
//
// Every caller of OnlyExec runs either a helper-script function or a
// mount/umount, and none of them has a legitimate reason to take minutes. It
// used to have no bound at all: `umount` blocked on writeback, a helper script
// waiting on a wedged device, or systemd taking its full stop timeout would
// each hold the handler — and whatever lock it had — with nothing to end it.
// Two minutes is above the slowest honest case here (systemd's 90 second
// TimeoutStopSec, which RestartSMBD waits out) and far below "never".
const defaultExecCeiling = 2 * time.Minute

// Deprecated: This method is not safe, sould have ensure input.
func OnlyExec(cmdStr string) (string, error) {
	return OnlyExecBounded(defaultExecCeiling, cmdStr)
}

// OnlyExecBounded is OnlyExec with the caller's own ceiling, for the commands
// whose honest duration differs from the default.
//
// WaitDelay matters as much as the timeout: killing the process does not end
// the wait if a child inherited the output pipe and kept it open, which is
// exactly what a hung mount helper does. Without it the context deadline fires
// and CombinedOutput still blocks forever — a bound that does not bind.
func OnlyExecBounded(timeout time.Duration, cmdStr string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/bash", "-c", cmdStr)
	cmd.WaitDelay = 5 * time.Second
	buf, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		// Say which command and how long, because the generic "signal: killed"
		// that surfaces otherwise names neither.
		return string(buf), fmt.Errorf("command timed out after %s and was killed: %s", timeout, cmdStr)
	}
	return string(buf), err
}

func ExecResultStr(cmdStr string) (string, error) {
	cmds := strings.Fields(cmdStr)
	if len(cmds) == 0 {
		return "", fmt.Errorf("empty command string")
	}
	cmd := exec2.Command(cmds[0], cmds[1:]...)

	output, err := cmd.CombinedOutput()
	return string(output), err
}

func ExecResultStrArray(cmdStr string) ([]string, error) {
	cmds := strings.Fields(cmdStr)
	if len(cmds) == 0 {
		return nil, fmt.Errorf("empty command string")
	}
	cmd := exec.Command(cmds[0], cmds[1:]...)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, err
	}

	result := strings.Split(string(output), "\n")
	return result, nil
}

func ExecuteScripts(scriptDirectory string) error {
	if _, err := os.Stat(scriptDirectory); os.IsNotExist(err) {
		fmt.Printf("No post-start scripts at %s\n", scriptDirectory)
		return err
	}

	files, err := os.ReadDir(scriptDirectory)
	if err != nil {
		fmt.Printf("Failed to read from script directory %s: %s\n", scriptDirectory, err.Error())
		return err
	}

	for _, file := range files {
		if file.IsDir() {
			continue
		}

		scriptFilepath := filepath.Join(scriptDirectory, file.Name())

		f, err := os.Open(scriptFilepath)
		if err != nil {
			fmt.Printf("Failed to open script file %s: %s\n", scriptFilepath, err.Error())
			continue
		}

		scanner := bufio.NewScanner(f)
		scanner.Scan()
		shebang := scanner.Text()
		f.Close()

		interpreter := "/bin/sh"
		if strings.HasPrefix(shebang, "#!") {
			interpreter = shebang[2:]
		}

		cmd := exec2.Command(interpreter, scriptFilepath)

		fmt.Printf("Executing post-start script %s using %s\n", scriptFilepath, interpreter)

		_, err = cmd.CombinedOutput()
		if err != nil {
			fmt.Printf("Failed to execute post-start script %s: %s\n", scriptFilepath, err.Error())
			return err
		}

	}
	fmt.Println("Finished executing post-start scripts.")

	return nil
}

func ExecStdin(stdinStr string, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	cmd.Stdin = bytes.NewBufferString(stdinStr)

	err := cmd.Run()
	if err != nil {
		fmt.Printf("Failed to execute command %s: %s\n", cmd.String(), err.Error())
		return "", err
	}

	return buf.String(), nil
}
