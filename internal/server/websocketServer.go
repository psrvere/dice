package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/dicedb/dice/config"
	"github.com/dicedb/dice/internal/clientio"
	"github.com/dicedb/dice/internal/ops"
	"github.com/dicedb/dice/internal/querywatcher"
	"github.com/dicedb/dice/internal/server/utils"
	"github.com/dicedb/dice/internal/shard"
	dstore "github.com/dicedb/dice/internal/store"
	"github.com/gorilla/websocket"
)

var unimplementedCommandsWebsocket map[string]bool = map[string]bool{
	"QWATCH":    true,
	"QUNWATCH":  true,
	"SUBSCRIBE": true,
	Abort:       false,
}

type WebsocketServer struct {
	querywatcher    *querywatcher.QueryManager
	shardManager    *shard.ShardManager
	ioChan          chan *ops.StoreResponse
	watchChan       chan dstore.WatchEvent
	websocketServer *http.Server
	upgrader        websocket.Upgrader
	logger          *slog.Logger
	shutdownChan    chan struct{}
}

func NewWebSocketServer(shardManager *shard.ShardManager, watchChan chan dstore.WatchEvent, logger *slog.Logger) *WebsocketServer {
	mux := http.NewServeMux()
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", config.WebsocketPort),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool { return true },
	}

	websocketServer := &WebsocketServer{
		shardManager:    shardManager,
		querywatcher:    querywatcher.NewQueryManager(logger),
		ioChan:          make(chan *ops.StoreResponse, 1000),
		watchChan:       watchChan,
		websocketServer: srv,
		upgrader:        upgrader,
		logger:          logger,
		shutdownChan:    make(chan struct{}),
	}

	mux.HandleFunc("/", websocketServer.WebsocketHandler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, err := w.Write([]byte("ok"))
		if err != nil {
			return
		}
	})
	return websocketServer
}

func (s *WebsocketServer) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	var err error

	websocketCtx, cancelWebsocket := context.WithCancel(ctx)
	defer cancelWebsocket()

	s.shardManager.RegisterWorker("wsServer", s.ioChan)

	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-ctx.Done():
		case <-s.shutdownChan:
			err = ErrAborted
			s.logger.Debug("Shutting down Websocket Server")
		}

		shutdownErr := s.websocketServer.Shutdown(websocketCtx)
		if shutdownErr != nil {
			s.logger.Error("Websocket Server shutdown failed:", slog.Any("error", err))
			return
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		s.logger.Info("Websocket Server running", slog.String("port", s.websocketServer.Addr[1:]))
		err = s.websocketServer.ListenAndServe()
	}()

	wg.Wait()
	return err
}

func (s *WebsocketServer) WebsocketHandler(w http.ResponseWriter, r *http.Request) {
	// upgrade http connection to websocket
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Error("Websocket upgrade failed", slog.Any("error", err))
	}
	// closing handshake
	defer func() {
		err := conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		if err != nil {
			s.logger.Error("Websocket close handshake failed", slog.Any("error", err))
		}
		conn.Close()
	}()

	// read incoming message
	_, msg, err := conn.ReadMessage()
	if err != nil {
		s.logger.Error("Websocket read failed", slog.Any("error", err))
		return
	}

	// parse message to dice command
	redisCmd, err := utils.ParseWebsocketMessage(msg)
	if err != nil {
		s.logger.Error("Error parsing Websocket request", slog.Any("error", err))
		return
	}

	if redisCmd.Cmd == Abort {
		s.logger.Debug("ABORT command received")
		s.logger.Debug("Shutting down Websocket Server")
		close(s.shutdownChan)
		return
	}

	if unimplementedCommandsWebsocket[redisCmd.Cmd] {
		s.logger.Error("Command is not implemented", slog.String("Command", redisCmd.Cmd))
		_, err := w.Write([]byte("Command is not implemented with Websocket"))
		if err != nil {
			s.logger.Error("Error writing response", slog.Any("error", err))
			return
		}
		return
	}

	// send request to Shard Manager
	s.shardManager.GetShard(0).ReqChan <- &ops.StoreOp{
		Cmd:         redisCmd,
		WorkerID:    "wsServer",
		ShardID:     0,
		WebsocketOp: true,
	}

	// Wait for response
	resp := <-s.ioChan

	var rp *clientio.RESPParser
	if resp.EvalResponse.Error != nil {
		rp = clientio.NewRESPParser(bytes.NewBuffer([]byte(resp.EvalResponse.Error.Error())))
	} else {
		rp = clientio.NewRESPParser(bytes.NewBuffer(resp.EvalResponse.Result.([]byte)))
	}

	val, err := rp.DecodeOne()
	if err != nil {
		s.logger.Error("Error decoding response", slog.Any("error", err))
		return
	}

	// Write response
	responseJSON, err := json.Marshal(val)
	if err != nil {
		s.logger.Error("Error marshaling response", slog.Any("error", err))
		return
	}
	err = conn.WriteMessage(websocket.TextMessage, responseJSON)
	if err != nil {
		s.logger.Error("Error writing response: %v", slog.Any("error", err))
		return
	}
}
