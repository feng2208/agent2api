package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"sync"
	"sync/atomic"
)

// RPCClient manages JSON-RPC 2.0 communication over io.Reader and io.Writer.
type RPCClient struct {
	in      io.Reader
	out     io.Writer
	outMu   sync.Mutex
	nextID  int64
	pending map[int64]chan *JSONRPCResponse
	pendMu  sync.Mutex

	// Handlers for incoming requests/notifications from the agent
	NotificationHandler func(req *JSONRPCRequest)
	RequestHandler      func(req *JSONRPCRequest) (json.RawMessage, *JSONRPCError)

	Debug bool // If true, logs all raw messages sent and received
}

func NewRPCClient(in io.Reader, out io.Writer) *RPCClient {
	return &RPCClient{
		in:      in,
		out:     out,
		pending: make(map[int64]chan *JSONRPCResponse),
	}
}

// Start begins reading from the input stream.
func (c *RPCClient) Start() {
	scanner := bufio.NewScanner(c.in)
	// ACP uses JSON-RPC over newline-delimited JSON (or similar).
	// Assuming newline-delimited JSON for stdio transport.
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024*10) // 10MB max line length

	for scanner.Scan() {
		line := scanner.Bytes()
		if c.Debug {
			log.Printf("[ACP RECV] %s", string(line))
		}
		c.handleMessage(line)
	}

	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		log.Printf("RPC client read error: %v", err)
	}

	// Cancel all pending requests
	c.pendMu.Lock()
	for _, ch := range c.pending {
		close(ch)
	}
	c.pending = make(map[int64]chan *JSONRPCResponse)
	c.pendMu.Unlock()
}

func (c *RPCClient) handleMessage(data []byte) {
	// A message could be a Request, a Notification, or a Response.
	var msg struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      *int64          `json:"id,omitempty"`
		Method  *string         `json:"method,omitempty"`
		Result  json.RawMessage `json:"result,omitempty"`
		Error   *JSONRPCError   `json:"error,omitempty"`
		Params  json.RawMessage `json:"params,omitempty"`
	}

	if err := json.Unmarshal(data, &msg); err != nil {
		log.Printf("Failed to unmarshal JSON-RPC message: %v", err)
		return
	}

	if msg.Method != nil {
		req := &JSONRPCRequest{
			JSONRPC: msg.JSONRPC,
			ID:      msg.ID,
			Method:  *msg.Method,
			Params:  msg.Params,
		}
		if msg.ID == nil {
			// Notification
			if c.NotificationHandler != nil {
				c.NotificationHandler(req)
			}
		} else {
			// Request from Agent to Gateway
			go c.handleIncomingRequest(req)
		}
	} else if msg.ID != nil {
		// Response from Agent to Gateway
		resp := &JSONRPCResponse{
			JSONRPC: msg.JSONRPC,
			ID:      *msg.ID,
			Result:  msg.Result,
			Error:   msg.Error,
		}
		c.pendMu.Lock()
		ch, ok := c.pending[*msg.ID]
		if ok {
			delete(c.pending, *msg.ID)
		}
		c.pendMu.Unlock()

		if ok {
			ch <- resp
		}
	}
}

func (c *RPCClient) handleIncomingRequest(req *JSONRPCRequest) {
	var result json.RawMessage
	var rpcErr *JSONRPCError

	if c.RequestHandler != nil {
		result, rpcErr = c.RequestHandler(req)
	} else {
		rpcErr = &JSONRPCError{Code: -32601, Message: "Method not found"}
	}

	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      *req.ID,
		Result:  result,
		Error:   rpcErr,
	}

	b, _ := json.Marshal(resp)
	c.sendMessage(b)
}

// Call makes a synchronous JSON-RPC request.
func (c *RPCClient) Call(ctx context.Context, method string, params any, result any) error {
	id := atomic.AddInt64(&c.nextID, 1)

	var rawParams json.RawMessage
	if params != nil {
		var err error
		rawParams, err = json.Marshal(params)
		if err != nil {
			return err
		}
	}

	req := JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  method,
		Params:  rawParams,
	}

	b, err := json.Marshal(req)
	if err != nil {
		return err
	}

	ch := make(chan *JSONRPCResponse, 1)
	c.pendMu.Lock()
	c.pending[id] = ch
	c.pendMu.Unlock()

	defer func() {
		c.pendMu.Lock()
		delete(c.pending, id)
		c.pendMu.Unlock()
	}()

	if err := c.sendMessage(b); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case resp, ok := <-ch:
		if !ok {
			return errors.New("connection closed")
		}
		if resp.Error != nil {
			return resp.Error
		}
		if result != nil && resp.Result != nil {
			return json.Unmarshal(resp.Result, result)
		}
		return nil
	}
}

// Notify sends a JSON-RPC notification.
func (c *RPCClient) Notify(method string, params any) error {
	var rawParams json.RawMessage
	if params != nil {
		var err error
		rawParams, err = json.Marshal(params)
		if err != nil {
			return err
		}
	}

	req := JSONRPCRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  rawParams,
	}

	b, err := json.Marshal(req)
	if err != nil {
		return err
	}

	return c.sendMessage(b)
}

func (c *RPCClient) sendMessage(data []byte) error {
	c.outMu.Lock()
	defer c.outMu.Unlock()

	if c.Debug {
		log.Printf("[ACP SEND] %s", string(data))
	}

	data = append(data, '\n')
	_, err := c.out.Write(data)
	return err
}
