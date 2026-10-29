package mcp

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxReply bounds what a tool reads back, so one huge page cannot flood the assistant's context.
const maxReply = 4 << 20

// HTTPDoer calls a wako server over HTTP with an API token.
type HTTPDoer struct {
	Base   string // e.g. "http://127.0.0.1:8888" (with the base path, if any)
	Token  string
	Client *http.Client
}

func (d *HTTPDoer) Do(ctx context.Context, method, path string, header http.Header, body []byte) (int, http.Header, []byte, error) {
	c := d.Client
	if c == nil {
		c = &http.Client{Timeout: 60 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(d.Base, "/")+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if d.Token != "" {
		req.Header.Set("Authorization", "Bearer "+d.Token)
	}
	req.Header.Set("User-Agent", "wako-mcp")
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxReply))
	return resp.StatusCode, resp.Header, data, err
}
