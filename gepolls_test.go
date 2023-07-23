package gepolls

import (
	"context"
	"net"
	"os"
	"testing"

	"golang.org/x/exp/slog"
)

var tests = []string{"hello", "お早う", "☀️"}

func TestNewServer(t *testing.T) {
	ctx := context.Background()

	programLevel := new(slog.LevelVar)
	programLevel.Set(slog.LevelDebug)

	handler := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: programLevel})

	s := newServer(ctx).WithAddress("0.0.0.0:4321").WithHandler(handler)

	if err := s.Start(); err != nil {
		t.Errorf("error: %v", err)
	}

	if conn, err := net.Dial("tcp", "0.0.0.0:4321"); err != nil {
		t.Errorf("error connecting %v", err)
	} else {
		for _, test := range tests {
			if _, err := conn.Write([]byte(test)); err != nil {
				t.Errorf("error writing to server")
			} else {
				res := <-s.DataChan
				if string(res.D) != test {
					t.Errorf("expected %s, got %s", test, string(res.D))
				}
			}
		}
	}
	s.Stop()
}
