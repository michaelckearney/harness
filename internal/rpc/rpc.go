// Package rpc is what a bot's harness command and the service say to each
// other over the socket: one JSON request, one JSON response.
package rpc

import (
	"encoding/json"
	"fmt"
	"net"
	"time"
)

type Request struct {
	// Cwd is where the command ran, which says whose turn it is.
	Cwd  string   `json:"cwd"`
	Cmd  string   `json:"cmd"`
	Args []string `json:"args"`
	// Body is Markdown, from a file or standard input.
	Body string `json:"body,omitempty"`
	// BodyDir is the folder of the body's file, or Cwd for standard input:
	// where the relative paths of its images start.
	BodyDir string `json:"body_dir,omitempty"`
}

type Response struct {
	Output string `json:"output"`
	// Code is the exit code: 0 done, 1 something the bot can fix, 2 the
	// service itself failed.
	Code int `json:"code"`
}

// Call sends one request. It waits as long as the service takes: a commit
// pushes, and a push can be slow.
func Call(socket string, req Request) (*Response, error) {
	conn, err := net.DialTimeout("unix", socket, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("the harness serve service is not running (%v). You cannot fix that from here: stop, and the owner will see it", err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, err
	}
	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, fmt.Errorf("the harness serve service did not answer: %v", err)
	}
	return &resp, nil
}
