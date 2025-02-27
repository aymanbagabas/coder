package pty_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/hinshun/vt10x"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/coder/coder/pty"
	"github.com/coder/coder/testutil"
)

// Test_Start_copy tests that we can use io.Copy() on command output
// without deadlocking.
func Test_Start_copy(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitShort)
	defer cancel()

	pc, cmd, err := pty.Start(pty.CommandContext(ctx, cmdEcho, argEcho...))
	require.NoError(t, err)
	b := &bytes.Buffer{}
	readDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(b, pc.OutputReader())
		readDone <- err
	}()

	select {
	case err := <-readDone:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Error("read timed out")
	}
	assert.Contains(t, b.String(), "test")

	cmdDone := make(chan error, 1)
	go func() {
		cmdDone <- cmd.Wait()
	}()

	select {
	case err := <-cmdDone:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Error("cmd.Wait() timed out")
	}
}

// Test_Start_truncation tests that we can read command output without truncation
// even after the command has exited.
func Test_Start_truncation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitSuperLong)
	defer cancel()

	pc, cmd, err := pty.Start(pty.CommandContext(ctx, cmdCount, argCount...))

	require.NoError(t, err)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		// avoid buffered IO so that we can precisely control how many bytes to read.
		n := 1
		for n <= countEnd {
			want := fmt.Sprintf("%d", n)
			err := readUntil(ctx, t, want, pc.OutputReader())
			assert.NoError(t, err, "want: %s", want)
			if err != nil {
				return
			}
			n++
			if (countEnd - n) < 100 {
				// If the OS buffers the output, the process can exit even if
				// we're not done reading.  We want to slow our reads so that
				// if there is a race between reading the data and it being
				// truncated, we will lose and fail the test.
				time.Sleep(testutil.IntervalFast)
			}
		}
		// ensure we still get to EOF
		endB := &bytes.Buffer{}
		_, err := io.Copy(endB, pc.OutputReader())
		assert.NoError(t, err)
	}()

	cmdDone := make(chan error, 1)
	go func() {
		cmdDone <- cmd.Wait()
	}()

	select {
	case err := <-cmdDone:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("cmd.Wait() timed out")
	}

	select {
	case <-readDone:
		// OK!
	case <-ctx.Done():
		t.Fatal("read timed out")
	}
}

// Test_Start_cancel_context tests that we can cancel the command context and kill the process.
func Test_Start_cancel_context(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), testutil.WaitMedium)
	defer cancel()
	cmdCtx, cmdCancel := context.WithCancel(ctx)

	pc, cmd, err := pty.Start(pty.CommandContext(cmdCtx, cmdSleep, argSleep...))
	require.NoError(t, err)
	defer func() {
		_ = pc.Close()
	}()
	cmdCancel()

	cmdDone := make(chan struct{})
	go func() {
		defer close(cmdDone)
		_ = cmd.Wait()
	}()

	select {
	case <-cmdDone:
		// OK!
	case <-ctx.Done():
		t.Error("cmd.Wait() timed out")
	}
}

// readUntil reads one byte at a time until we either see the string we want, or the context expires
func readUntil(ctx context.Context, t *testing.T, want string, r io.Reader) error {
	// output can contain virtual terminal sequences, so we need to parse these
	// to correctly interpret getting what we want.
	term := vt10x.New(vt10x.WithSize(80, 80))
	readErrs := make(chan error, 1)
	for {
		b := make([]byte, 1)
		go func() {
			_, err := r.Read(b)
			readErrs <- err
		}()
		select {
		case err := <-readErrs:
			if err != nil {
				t.Logf("err: %v\ngot: %v", err, term)
				return err
			}
			term.Write(b)
		case <-ctx.Done():
			return ctx.Err()
		}
		got := term.String()
		lines := strings.Split(got, "\n")
		for _, line := range lines {
			if strings.TrimSpace(line) == want {
				t.Logf("want: %v\n got:%v", want, line)
				return nil
			}
		}
	}
}
