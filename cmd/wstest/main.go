// Command wstest is a tiny end-to-end terminal client used to verify the
// browser↔server↔agent PTY path without a real browser. It:
//  1. POSTs /api/v1/hosts/:id/terminals to open a session
//  2. Opens the returned ws_url as a WebSocket
//  3. Sends a base64 "input" frame (a shell command)
//  4. Reads "output" frames for a few seconds and prints decoded text
//
// Run from the repo root:  go run ./cmd/wstest -addr :8080 -host <agentID>
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

type frame struct {
	Type   string `json:"type"`
	Sid    string `json:"sid,omitempty"`
	Data   string `json:"data,omitempty"`
	Cols   uint32 `json:"cols,omitempty"`
	Rows   uint32 `json:"rows,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func main() {
	addr := flag.String("addr", "localhost:8080", "control server host:port")
	hostID := flag.String("host", "", "agent id")
	cmd := flag.String("cmd", "echo hello-from-pty && whoami && ver\r", "input to send (use \\r for Enter)")
	wait := flag.Duration("wait", 4*time.Second, "how long to read output")
	flag.Parse()
	if *hostID == "" {
		log.Fatal("must set -host")
	}

	base := "http://" + *addr
	openURL := fmt.Sprintf("%s/api/v1/hosts/%s/terminals", base, *hostID)

	// Open a terminal session via REST.
	req, _ := http.NewRequest("POST", openURL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatalf("open terminal: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Fatalf("open terminal status %d", resp.StatusCode)
	}
	var open struct {
		SessionID string `json:"session_id"`
		WSURL     string `json:"ws_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&open); err != nil {
		log.Fatalf("decode open resp: %v", err)
	}
	log.Printf("opened session %s ws=%s", open.SessionID, open.WSURL)

	// Connect WebSocket.
	wsURL := "ws://" + *addr + open.WSURL
	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		log.Fatalf("ws dial: %v", err)
	}
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), *wait)
	defer cancel()

	// Give the agent a moment to spawn the PTY, then send input.
	go func() {
		time.Sleep(600 * time.Millisecond)
		input := unescape(*cmd)
		in := frame{Type: "input", Sid: open.SessionID, Data: b64(input)}
		if err := c.WriteJSON(in); err != nil {
			log.Printf("ws write: %v", err)
		}
	}()

	// Read frames until timeout.
	c.SetReadDeadline(time.Now().Add(*wait))
	fmt.Println("----- terminal output -----")
	for {
		var f frame
		if err := c.ReadJSON(&f); err != nil {
			break
		}
		switch f.Type {
		case "output":
			if f.Data != "" {
				out, _ := b64decode(f.Data)
				fmt.Print(out)
			}
		case "ended":
			fmt.Printf("\n[ended reason=%q]\n", f.Reason)
			return
		case "pong":
		}
		if ctx.Err() != nil {
			break
		}
	}
	fmt.Println("\n----- end -----")
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
func b64decode(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// unescape turns literal \r / \n in the -cmd flag into real control chars.
func unescape(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'r':
				out = append(out, '\r')
				i++
				continue
			case 'n':
				out = append(out, '\n')
				i++
				continue
			}
		}
		out = append(out, s[i])
	}
	return string(out)
}