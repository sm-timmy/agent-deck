//go:build !windows
// +build !windows

package tmux

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	"golang.org/x/term"
)

// IndexDetachKey returns the index of a control-key sequence in data, or -1 if
// not found. detachByte is the raw ASCII byte (e.g. 0x11 for Ctrl+Q).
// Handles three encodings:
//   - Raw byte
//   - xterm modifyOtherKeys: ESC[27;5;{keyCode}~
//   - CSI u (kitty keyboard protocol): ESC[{keyCode};5u
func IndexDetachKey(data []byte, detachByte byte) int {
	if idx := bytes.IndexByte(data, detachByte); idx >= 0 {
		return idx
	}
	// Derive the printable key code for escape sequence matching.
	var keyCode byte
	if detachByte >= 1 && detachByte <= 26 {
		keyCode = detachByte + 96 // ctrl+letter: 1-26 -> 'a'-'z'
	} else if detachByte >= 28 && detachByte <= 31 {
		keyCode = detachByte + 64 // ctrl+special: 28-31 -> '\',']','^','_'
	}
	if keyCode > 0 {
		modSeq := fmt.Sprintf("\x1b[27;5;%d~", keyCode)
		if idx := bytes.Index(data, []byte(modSeq)); idx >= 0 {
			return idx
		}
		csiSeq := fmt.Sprintf("\x1b[%d;5u", keyCode)
		if idx := bytes.Index(data, []byte(csiSeq)); idx >= 0 {
			return idx
		}
	}
	return -1
}

// IndexCtrlQ returns the index of a Ctrl+Q sequence in data, or -1 if not found.
// This is a convenience wrapper around IndexDetachKey with the default Ctrl+Q byte.
func IndexCtrlQ(data []byte) int {
	return IndexDetachKey(data, 17)
}

// Attach attaches to the tmux session with full PTY support.
// The configured detach key (default Ctrl+Q) will detach and return to the caller.
// Pass an optional detachByte to override the default (0x11 / Ctrl+Q).
func (s *Session) Attach(ctx context.Context, detachByte ...byte) error {
	_ = detachByte

	if !s.Exists() {
		return fmt.Errorf("session %s does not exist", s.Name)
	}

	// Use tmux's native attach path directly. The custom PTY attach path causes
	// fullscreen Codex sessions to redraw in a way that collapses pane history
	// on reattach, while direct `tmux attach-session` preserves scrollback.
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("failed to set raw mode: %w", err)
	}
	defer func() { _ = term.Restore(int(os.Stdin.Fd()), oldState) }()
	cmd := exec.CommandContext(ctx, "tmux", "attach-session", "-t", s.Name)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to attach to session: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if exitErr.ExitCode() == 0 || exitErr.ExitCode() == 1 {
				return nil
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("attach command failed: %w", err)
	}

	return nil
}

// AttachWindow attaches to a specific window within this tmux session.
// Selects the target window first, then uses the standard Attach flow.
func (s *Session) AttachWindow(ctx context.Context, windowIndex int, detachByte ...byte) error {
	if !s.Exists() {
		return fmt.Errorf("session %s does not exist", s.Name)
	}

	// Select the target window before attaching
	target := fmt.Sprintf("%s:%d", s.Name, windowIndex)
	if err := exec.Command("tmux", "select-window", "-t", target).Run(); err != nil {
		return fmt.Errorf("failed to select window %s: %w", target, err)
	}

	return s.Attach(ctx, detachByte...)
}

// Resize changes the terminal size of the tmux session
func (s *Session) Resize(cols, rows int) error {
	// Resize the tmux window
	cmd := exec.Command("tmux", "resize-window", "-t", s.Name, "-x", fmt.Sprintf("%d", cols), "-y", fmt.Sprintf("%d", rows))
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to resize window: %w", err)
	}
	return nil
}

// AttachReadOnly attaches to the session in read-only mode
func (s *Session) AttachReadOnly(ctx context.Context) error {
	if !s.Exists() {
		return fmt.Errorf("session %s does not exist", s.Name)
	}

	// Save original terminal state
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		return fmt.Errorf("failed to set raw mode: %w", err)
	}
	defer func() { _ = term.Restore(int(os.Stdin.Fd()), oldState) }()

	// Start tmux attach command in read-only mode
	cmd := exec.CommandContext(ctx, "tmux", "attach-session", "-r", "-t", s.Name)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Start the attach command
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to attach to session: %w", err)
	}

	// Wait for command to finish
	if err := cmd.Wait(); err != nil {
		// Check if it's a normal detach
		if exitErr, ok := err.(*exec.ExitError); ok {
			if exitErr.ExitCode() == 0 || exitErr.ExitCode() == 1 {
				return nil
			}
		}
		return fmt.Errorf("attach command failed: %w", err)
	}

	return nil
}

// StreamOutput streams the session output to the provided writer
func (s *Session) StreamOutput(ctx context.Context, w io.Writer) error {
	if !s.Exists() {
		return fmt.Errorf("session %s does not exist", s.Name)
	}

	// Use tmux pipe-pane to stream output
	cmd := exec.CommandContext(ctx, "tmux", "pipe-pane", "-t", s.Name, "-o", "cat")
	cmd.Stdout = w
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start pipe-pane: %w", err)
	}

	// Wait for context cancellation or command completion
	// Use WaitGroup to prevent goroutine leak on context cancellation
	var wg sync.WaitGroup
	errChan := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		errChan <- cmd.Wait()
	}()

	select {
	case <-ctx.Done():
		// Stop pipe-pane - error is intentionally ignored since we're
		// already returning ctx.Err() and cleanup failure is non-fatal
		stopCmd := exec.Command("tmux", "pipe-pane", "-t", s.Name)
		_ = stopCmd.Run()
		// Wait for the goroutine to complete before returning
		wg.Wait()
		return ctx.Err()
	case err := <-errChan:
		if err != nil {
			return fmt.Errorf("pipe-pane failed: %w", err)
		}
		return nil
	}
}
