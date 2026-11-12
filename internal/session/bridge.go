package session

import (
	"fmt"
	"io"
	"net"
)

// Bridge is `silo mcp-bridge`: the agent CLI starts it as its stdio MCP
// server. It copies r to the session router's socket and the socket to w until either ends.
func Bridge(socket string, r io.Reader, w io.Writer) error {
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return fmt.Errorf("connect to the silo session: %w", err)
	}
	defer conn.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(conn, r); conn.(*net.UnixConn).CloseWrite(); done <- struct{}{} }()
	go func() { io.Copy(w, conn); done <- struct{}{} }()
	<-done
	return nil
}
