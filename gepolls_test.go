package gepolls

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"
)

var tests = []string{"hello", "お早う", "☀️"}

func TestNewServer(t *testing.T) {
	ctx := context.Background()

	programLevel := new(slog.LevelVar)
	programLevel.Set(slog.LevelDebug)

	handler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: programLevel})

	s := NewServer(ctx).WithAddress("0.0.0.0:4322").WithHandler(handler)

	if err := s.Start(); err != nil {
		t.Errorf("error starting: %v", err)
		fmt.Println(err)
	}

	if conn, err := net.Dial("tcp", "0.0.0.0:4322"); err != nil {
		t.Errorf("error connecting %v", err)
	} else {
		in := bufio.NewReader(conn)

		sig := <-s.DataChan

		if sig.Type != ClientConnect {
			t.Errorf("did not get client connect message")
		}

		for _, test := range tests {
			if _, err := conn.Write([]byte(test)); err != nil {
				t.Errorf("error writing to server")
			} else {
				res := <-s.DataChan
				if string(res.Data) != test {
					t.Errorf("expected receive %s, got %s", test, string(res.Data))
				} else {
					if err := s.WriteAll(res.Client, res.Data); err != nil {
						t.Errorf("error writing: %v", err)
					} else {
						conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
						if resp, _, err := in.ReadLine(); err != nil {
							t.Errorf("error reading: %v", err)
						} else {
							if string(resp) != test {
								t.Errorf("expected read %s, got %s", test, string(resp))
							}
						}
					}
				}
			}
		}
		conn.Close()

		sig = <-s.DataChan

		if sig.Type != ClientDisconnect {
			t.Errorf("did not get client disconnect message")
		}
	}
	s.Stop()
}
