// Package rpc implements the gRPC server that agents dial into.
package rpc

import (
	"context"
	"io"
	"log/slog"
	"time"

	"watchman/proto/agentpb"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Server implements agentpb.AgentServiceServer.
type Server struct {
	agentpb.UnimplementedAgentServiceServer

	registry *Registry
	log      *slog.Logger
}

func NewServer(reg *Registry, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{registry: reg, log: log}
}

// Connect is the single bidirectional stream every agent opens on startup
// and on each reconnect. The agent sends a RegisterRequest first (either with
// an enroll_token on first registration, or with just its agent_id when
// reconnecting using an existing auth_token sent via metadata), then heartbeats
// and business messages. The server may push commands back at any time.
func (s *Server) Connect(stream agentpb.AgentService_ConnectServer) error {
	ctx := stream.Context()

	// First message MUST be RegisterRequest.
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.Unauthenticated, "recv register: %v", err)
	}
	reg := first.GetRegister()
	if reg == nil {
		return status.Error(codes.InvalidArgument, "first message must be RegisterRequest")
	}

	// Reconnecting agents carry their auth_token in metadata.
	md, _ := metadata.FromIncomingContext(ctx)
	authToken := firstOf(md, "authorization")

	hub, resp, err := s.registry.Register(ctx, reg, authToken)
	if err != nil {
		s.log.Warn("agent register rejected", "agent_id", reg.GetAgentId(), "err", err)
		return err
	}

	// Send RegisterResponse.
	if err := stream.Send(&agentpb.ServerMessage{
		Payload: &agentpb.ServerMessage_Register{Register: resp},
	}); err != nil {
		return err
	}
	if !resp.GetOk() {
		return status.Error(codes.PermissionDenied, resp.GetError())
	}

	s.log.Info("agent connected", "agent_id", hub.AgentID, "hostname", reg.GetHostname())
	defer s.log.Info("agent disconnected", "agent_id", hub.AgentID)

	// Register the stream with the hub so the server can push messages, and
	// clean up on exit.
	hub.bind(stream)
	defer hub.unbind()

	// Pump agent -> server messages.
	for {
		msg, err := stream.Recv()
		if err != nil {
			if err == io.EOF || ctx.Err() != nil {
				return nil
			}
			s.log.Debug("agent stream recv error", "agent_id", hub.AgentID, "err", err)
			return err
		}
		if err := hub.handleAgentMessage(msg); err != nil {
			s.log.Warn("handle agent message", "agent_id", hub.AgentID, "err", err)
		}
	}
}

func firstOf(md metadata.MD, key string) string {
	if len(md[key]) == 0 {
		return ""
	}
	return md[key][0]
}

// keepaliveTick is how often the registry reaps stale agents.
const keepaliveTick = 10 * time.Second

// StartReaper periodically marks agents offline when their last-seen exceeds
// the heartbeat window. Call once at startup; cancel ctx to stop.
func (s *Server) StartReaper(ctx context.Context) {
	t := time.NewTicker(keepaliveTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.registry.ReapStale()
		}
	}
}