// Copyright (c) 2026 PicoClaw contributors
// SPDX-License-Identifier: MIT
// Ported from github.com/sipeed/picoclaw/pkg/mcp/isolated_command_transport.go

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const defaultTerminateDuration = 5 * time.Second

// stdioTransport implements mcp.Transport for subprocess-based MCP servers.
// Ported from picoclaw/pkg/mcp/isolated_command_transport.go (MIT).
type stdioTransport struct {
	Command           *exec.Cmd
	TerminateDuration time.Duration
}

func (t *stdioTransport) Connect(ctx context.Context) (sdkmcp.Connection, error) {
	stdout, err := t.Command.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stdout = io.NopCloser(stdout)
	stdin, err := t.Command.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := t.Command.Start(); err != nil {
		return nil, err
	}
	td := t.TerminateDuration
	if td <= 0 {
		td = defaultTerminateDuration
	}
	rwc := &stdioPipeRWC{
		cmd:               t.Command,
		stdout:            stdout,
		stdin:             stdin,
		terminateDuration: td,
	}
	return newStdioConn(rwc), nil
}

type stdioPipeRWC struct {
	cmd               *exec.Cmd
	stdout            io.ReadCloser
	stdin             io.WriteCloser
	terminateDuration time.Duration
}

func (s *stdioPipeRWC) Read(p []byte) (n int, err error) {
	return s.stdout.Read(p)
}

func (s *stdioPipeRWC) Write(p []byte) (n int, err error) {
	return s.stdin.Write(p)
}

func (s *stdioPipeRWC) Close() error {
	if err := s.stdin.Close(); err != nil {
		return fmt.Errorf("closing stdin: %w", err)
	}
	resChan := make(chan error, 1)
	go func() {
		resChan <- s.cmd.Wait()
	}()
	wait := func() (error, bool) {
		select {
		case err := <-resChan:
			return err, true
		case <-time.After(s.terminateDuration):
		}
		return nil, false
	}
	if err, ok := wait(); ok {
		return err
	}
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err == nil {
		if err, ok := wait(); ok {
			return err
		}
	}
	if err := s.cmd.Process.Kill(); err != nil {
		return err
	}
	if err, ok := wait(); ok {
		return err
	}
	return fmt.Errorf("unresponsive subprocess")
}

type stdioConn struct {
	writeMu   sync.Mutex
	rwc       io.ReadWriteCloser
	incoming  <-chan msgOrErr
	queue     []jsonrpc.Message
	closeOnce sync.Once
	closed    chan struct{}
	closeErr  error
}

type msgOrErr struct {
	msg json.RawMessage
	err error
}

func newStdioConn(rwc io.ReadWriteCloser) *stdioConn {
	incoming := make(chan msgOrErr)
	closed := make(chan struct{})
	go func() {
		dec := json.NewDecoder(rwc)
		for {
			var raw json.RawMessage
			err := dec.Decode(&raw)
			if err == nil {
				var tr [1]byte
				if n, readErr := dec.Buffered().Read(tr[:]); n > 0 {
					if tr[0] != '\n' && tr[0] != '\r' {
						err = fmt.Errorf("invalid trailing data at the end of stream")
					}
				} else if readErr != nil && readErr != io.EOF {
					err = readErr
				}
			}
			select {
			case incoming <- msgOrErr{msg: raw, err: err}:
			case <-closed:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return &stdioConn{rwc: rwc, incoming: incoming, closed: closed}
}

func (c *stdioConn) SessionID() string { return "" }

func (c *stdioConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	if len(c.queue) > 0 {
		next := c.queue[0]
		c.queue = c.queue[1:]
		return next, nil
	}
	var raw json.RawMessage
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case v := <-c.incoming:
		if v.err != nil {
			return nil, v.err
		}
		raw = v.msg
	case <-c.closed:
		return nil, io.EOF
	}
	msgs, err := readBatch(raw)
	if err != nil {
		return nil, err
	}
	c.queue = msgs[1:]
	return msgs[0], nil
}

func readBatch(data []byte) ([]jsonrpc.Message, error) {
	var rawBatch []json.RawMessage
	if err := json.Unmarshal(data, &rawBatch); err == nil {
		if len(rawBatch) == 0 {
			return nil, fmt.Errorf("empty batch")
		}
		msgs := make([]jsonrpc.Message, 0, len(rawBatch))
		for _, raw := range rawBatch {
			msg, err := jsonrpc.DecodeMessage(raw)
			if err != nil {
				return nil, err
			}
			msgs = append(msgs, msg)
		}
		return msgs, nil
	}
	msg, err := jsonrpc.DecodeMessage(data)
	if err != nil {
		return nil, err
	}
	return []jsonrpc.Message{msg}, nil
}

func (c *stdioConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	data, err := jsonrpc.EncodeMessage(msg)
	if err != nil {
		return fmt.Errorf("marshaling message: %w", err)
	}
	data = append(data, '\n')
	_, err = c.rwc.Write(data)
	return err
}

func (c *stdioConn) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = c.rwc.Close()
		close(c.closed)
	})
	return c.closeErr
}

var (
	_ sdkmcp.Transport  = (*stdioTransport)(nil)
	_ sdkmcp.Connection = (*stdioConn)(nil)
)
