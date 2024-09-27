package websocket

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/dicedb/dice/internal/logger"
)

func TestMain(m *testing.M) {
	logger := logger.New(logger.Opts{WithTimestamp: false})
	slog.SetDefault(logger)
	var wg sync.WaitGroup

	// Run the test server
	// This is a synchronous method, because internally it
	// checks for available port and then forks a goroutine
	// to start the server
	opts := TestServerOptions{
		Port:   8380,
		Logger: logger,
	}
	RunWebsocketServer(context.Background(), &wg, opts)

	// Wait for the server to start
	time.Sleep(2 * time.Second)

	executor := NewWebsocketCommandExecutor()

	// Run the test suite
	exitCode := m.Run()

	executor.FireCommand(WebsocketCommand{
		Message: "abort",
	})

	wg.Wait()
	os.Exit(exitCode)
}
