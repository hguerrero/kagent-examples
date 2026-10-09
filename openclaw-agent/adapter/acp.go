package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
)

// message is one JSON-RPC 2.0 frame. A frame with a method and an ID is a
// request, a frame with a method only is a notification, and a frame with an ID
// and no method is a response.
type message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("acp error %d: %s", e.Code, e.Message) }

func (m message) isResponse() bool { return m.Method == "" && len(m.ID) > 0 }

var errBridgeExited = errors.New("the openclaw acp bridge exited")

// acpClient speaks the Agent Client Protocol to `openclaw acp` over stdio.
//
// Every frame the bridge writes lands on one ordered channel, responses
// included, so a turn sees its updates before its final response. There is a
// single consumer: kagent runs one task at a time in an actor.
type acpClient struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	writes sync.Mutex
	nextID atomic.Int64
	frames chan message
	done   chan struct{} // closed when the bridge process has exited
}

func startACP(cmd *exec.Cmd) (*acpClient, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &acpClient{cmd: cmd, stdin: stdin, frames: make(chan message, 1024), done: make(chan struct{})}
	go func() {
		defer close(c.done)
		defer close(c.frames)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 32<<20) // tool output can make long lines
		for sc.Scan() {
			var m message
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				continue // not a frame: ignore stray output
			}
			c.frames <- m
		}
		_ = cmd.Wait()
	}()
	return c, nil
}

func (c *acpClient) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writes.Lock()
	defer c.writes.Unlock()
	_, err = c.stdin.Write(append(b, '\n'))
	return err
}

// send writes a request and returns its ID. The caller reads the response from
// the frame stream.
func (c *acpClient) send(method string, params any) (int64, error) {
	id := c.nextID.Add(1)
	return id, c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
}

func (c *acpClient) notify(method string, params any) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// reply answers a request the bridge made, such as session/request_permission.
func (c *acpClient) reply(id json.RawMessage, result any) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// next returns the next frame, or an error once the bridge has gone.
func (c *acpClient) next(ctx context.Context) (message, error) {
	select {
	case <-ctx.Done():
		return message{}, ctx.Err()
	case m, ok := <-c.frames:
		if !ok {
			return message{}, errBridgeExited
		}
		return m, nil
	}
}

// call sends a request and waits for its response, skipping other frames. Use
// it only outside a turn, where no updates are expected.
func (c *acpClient) call(ctx context.Context, method string, params, result any) error {
	id, err := c.send(method, params)
	if err != nil {
		return err
	}
	want := fmt.Sprint(id)
	for {
		m, err := c.next(ctx)
		if err != nil {
			return err
		}
		if !m.isResponse() || string(m.ID) != want {
			continue
		}
		if m.Error != nil {
			return m.Error
		}
		if result != nil {
			return json.Unmarshal(m.Result, result)
		}
		return nil
	}
}

func (c *acpClient) close() {
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	<-c.done
}
