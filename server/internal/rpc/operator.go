// Operator tunnel data plane (§3.9.1 按需运维隧道).
//
// OperatorService is a user-authenticated gRPC service served on its own
// dedicated listener (-opgrpc, TLS mandatory), physically separate from the
// web panel port and the agent-facing port. One bidirectional stream proxies
// exactly one local TCP connection from the watchmanctl CLI; the server
// bridges the stream to a listener-less agent tunnel leg (Coordinator.OpenLeg)
// using the TunnelOpen/TunnelData/TunnelClose frames of the agent stream.
//
// Frame flow on one Connect stream:
//
//	client -> server: OperatorOpen (must be the first frame)
//	server -> client: OperatorOpenAck{ok, error, conn_id}
//	both ways:        OperatorData{data}
//	either way:       OperatorClose{reason}
//
// Every open enforces role (viewer denied), group visibility, an app-scoped
// port allowlist, per-user concurrency limits, and audits open/close/deny
// with the operator identity. No business bytes ever touch the panel port.
package rpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"watchman/proto/agentpb"
	"watchman/server/internal/audit"
	"watchman/server/internal/auth"
)

// maxLegsPerOperator caps simultaneously open tunnel legs of one user.
const maxLegsPerOperator = 8

// operatorClaimsKey is the stream-context key holding the validated user.
type operatorClaimsKey struct{}

// OperatorAuthInterceptor validates the Bearer JWT in gRPC metadata for
// every OperatorService stream and injects the claims into the stream
// context. Other services pass through untouched.
func OperatorAuthInterceptor(authStore *auth.Store) grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if !strings.HasPrefix(info.FullMethod, "/watchman.agent.OperatorService/") {
			return handler(srv, ss)
		}
		md, ok := metadata.FromIncomingContext(ss.Context())
		if !ok {
			return status.Error(codes.Unauthenticated, "missing metadata")
		}
		var token string
		for _, v := range md.Get("authorization") {
			if t := stripBearer(v); t != "" {
				token = t
				break
			}
		}
		if token == "" {
			return status.Error(codes.Unauthenticated, "missing bearer token")
		}
		claims, err := authStore.Validate(token)
		if err != nil {
			return status.Error(codes.Unauthenticated, "invalid token: "+err.Error())
		}
		ctx := context.WithValue(ss.Context(), operatorClaimsKey{}, claims)
		return handler(srv, &operatorStream{ServerStream: ss, ctx: ctx})
	}
}

// operatorStream wraps a server stream with an augmented context (claims).
type operatorStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w *operatorStream) Context() context.Context { return w.ctx }

func operatorClaimsFrom(ctx context.Context) (*auth.Claims, bool) {
	claims, ok := ctx.Value(operatorClaimsKey{}).(*auth.Claims)
	return claims, ok && claims != nil
}

func operatorPeerIP(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return ""
}

// AppPortInfo describes one exposed application port for allowlist checks.
type AppPortInfo struct {
	Port int
	Mesh bool // bind_scope == mesh: reachable at the host's Tailscale IP
}

// AppTargetInfo is the resolved view of an application for tunnel opens.
type AppTargetInfo struct {
	HostID string
	Name   string
	Ports  []AppPortInfo
}

// OperatorServiceDeps wires the operator data plane into server stores
// without importing packages that already depend on rpc (import cycles).
type OperatorServiceDeps struct {
	Registry    *Registry
	Coordinator *Coordinator
	Audit       *audit.Store
	Log         *slog.Logger
	// CanAccessGroup mirrors groups.Store.CanAccessGroup.
	CanAccessGroup func(username string, isAdmin bool, group string) bool
	// ResolveApp mirrors apps.Store.GetApp plus PortMapping flattening.
	ResolveApp func(appID string) (AppTargetInfo, bool)
	// MeshIP returns the host's cached Tailscale IPv4 ("" when absent),
	// matching the convention in apps/proxy.go.
	MeshIP func(hostID string) string
}

// OperatorService implements agentpb.OperatorServiceServer.
type OperatorService struct {
	agentpb.UnimplementedOperatorServiceServer

	deps OperatorServiceDeps
	log  *slog.Logger
}

// NewOperatorService creates the operator data-plane service.
func NewOperatorService(deps OperatorServiceDeps) *OperatorService {
	log := deps.Log
	if log == nil {
		log = slog.Default()
	}
	return &OperatorService{deps: deps, log: log}
}

// Connect handles one operator tunnel stream (one proxied TCP connection).
func (s *OperatorService) Connect(stream grpc.BidiStreamingServer[agentpb.OperatorMessage, agentpb.OperatorMessage]) error {
	ctx := stream.Context()
	claims, ok := operatorClaimsFrom(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing authenticated identity")
	}
	ip := operatorPeerIP(ctx)

	// Role gate is cheaper than reading the first frame: viewers never tunnel.
	if claims.Role != auth.RoleAdmin && claims.Role != auth.RoleOperator {
		return s.deny(claims, ip, "", "", "viewer 角色不允许使用运维隧道", audit.RiskMedium)
	}

	first, err := stream.Recv()
	if err != nil {
		return nil // client hung up before opening; nothing to audit
	}
	open := first.GetOpen()
	if open == nil {
		return status.Error(codes.FailedPrecondition, "first frame must be OperatorOpen")
	}

	hostID, targetHost, targetPort, targetType, targetID, desc, risk, derr := s.resolveTarget(open, claims)
	if derr != nil {
		return s.deny(claims, ip, targetType, targetID, derr.Error(), risk)
	}

	// Both leg caps are enforced atomically inside OpenLeg; a separate
	// pre-check here would race a concurrent open.
	hub := s.deps.Registry.Hub(hostID)
	connID, inbound, err := s.deps.Coordinator.OpenLeg(hub, targetHost, targetPort, claims.Username, desc, maxLegsPerOperator)
	if err != nil {
		if errors.Is(err, ErrOwnerLegsExceeded) || errors.Is(err, ErrAgentLegsExceeded) {
			return s.deny(claims, ip, targetType, targetID, err.Error(), audit.RiskMedium)
		}
		// Infrastructure failure, not a policy denial: audit a failed open
		// (op_tunnel_open/failed), never op_tunnel_deny, and tell the client
		// it's transient (Unavailable) rather than refused.
		s.deps.Audit.Record(audit.Entry{
			Username:   claims.Username,
			Action:     "op_tunnel_open",
			TargetType: targetType,
			TargetID:   targetID,
			Detail:     fmt.Sprintf("打开运维隧道失败 %s → %s:%d: %v", desc, targetHost, targetPort, err),
			IP:         ip,
			RiskLevel:  audit.RiskLow,
			Result:     audit.ResultFailed,
		})
		return status.Error(codes.Unavailable, "建立隧道腿失败: "+err.Error())
	}
	leg := s.deps.Coordinator.legByID(connID)
	if leg == nil {
		s.deps.Coordinator.dropLeg(connID)
		return status.Error(codes.Internal, "tunnel leg vanished after open")
	}

	s.deps.Audit.Record(audit.Entry{
		Username:   claims.Username,
		Action:     "op_tunnel_open",
		TargetType: targetType,
		TargetID:   targetID,
		Detail:     fmt.Sprintf("打开运维隧道 %s → %s:%d", desc, targetHost, targetPort),
		IP:         ip,
		RiskLevel:  risk,
		Result:     audit.ResultSuccess,
	})
	s.log.Info("operator tunnel opened", "user", claims.Username, "conn", connID,
		"host", hostID, "target", fmt.Sprintf("%s:%d", targetHost, targetPort), "ip", ip)

	// Ack is the last message sent outside the agent->client pump goroutine
	// (gRPC allows concurrent Recv+Send but not concurrent Send+Send).
	if err := stream.Send(&agentpb.OperatorMessage{Payload: &agentpb.OperatorMessage_Ack{
		Ack: &agentpb.OperatorOpenAck{Ok: true, ConnId: connID},
	}}); err != nil {
		s.deps.Coordinator.dropLeg(connID)
		// The open was already audited as success: pair it with a close entry
		// so the audit log never shows a tunnel left dangling.
		s.deps.Audit.Record(audit.Entry{
			Username:   claims.Username,
			Action:     "op_tunnel_close",
			TargetType: targetType,
			TargetID:   targetID,
			Detail:     fmt.Sprintf("关闭运维隧道 %s → %s:%d (ack 发送失败: %v)", desc, targetHost, targetPort, err),
			IP:         ip,
			RiskLevel:  audit.RiskLow,
			Result:     audit.ResultSuccess,
		})
		return nil
	}

	errCh := make(chan error, 2)
	go s.pumpClientToAgent(stream, hub, connID, leg, errCh)
	go s.pumpAgentToClient(stream, inbound, leg, errCh)

	err = <-errCh
	s.deps.Coordinator.dropLeg(connID) // also unblocks the other pump via done

	lifetime := time.Since(leg.createdAt).Round(time.Second)
	reason := "会话结束"
	if err != nil {
		reason = err.Error()
	}
	s.deps.Audit.Record(audit.Entry{
		Username:   claims.Username,
		Action:     "op_tunnel_close",
		TargetType: targetType,
		TargetID:   targetID,
		Detail:     fmt.Sprintf("关闭运维隧道 %s → %s:%d (存活 %s, %s)", desc, targetHost, targetPort, lifetime, reason),
		IP:         ip,
		RiskLevel:  audit.RiskLow,
		Result:     audit.ResultSuccess,
	})
	s.log.Info("operator tunnel closed", "user", claims.Username, "conn", connID, "lifetime", lifetime, "reason", reason)
	return nil
}

// resolveTarget validates an OperatorOpen against role/group/port rules and
// resolves host, target and audit metadata. Returned risk is for the deny
// audit when err != nil, and for the open audit otherwise.
func (s *OperatorService) resolveTarget(open *agentpb.OperatorOpen, claims *auth.Claims) (hostID, targetHost string, targetPort uint32, targetType, targetID, desc, risk string, err error) {
	isAdmin := claims.Role == auth.RoleAdmin
	targetPort = open.GetTargetPort()
	if targetPort == 0 || targetPort > 65535 {
		return "", "", 0, "", "", "", audit.RiskLow, fmt.Errorf("必须指定合法目标端口 (1~65535)")
	}

	targetType, targetID = "host", open.GetHostId()
	if open.GetAppId() != "" {
		// Denial messages are deliberately identical to the group-visibility
		// denial below: a cross-group caller must not learn whether an app ID
		// exists by comparing error texts (existence oracle).
		app, ok := s.deps.ResolveApp(open.GetAppId())
		if !ok {
			return "", "", 0, "app", open.GetAppId(), "", audit.RiskMedium, fmt.Errorf("目标不存在或无权访问")
		}
		hostID, targetType, targetID = app.HostID, "app", open.GetAppId()
		desc = fmt.Sprintf("应用 %s", app.Name)
		var matched *AppPortInfo
		for i := range app.Ports {
			if uint32(app.Ports[i].Port) == open.GetTargetPort() {
				matched = &app.Ports[i]
				break
			}
		}
		if matched == nil {
			return "", "", 0, targetType, targetID, desc, audit.RiskMedium,
				fmt.Errorf("端口 %d 不在应用暴露端口白名单内", open.GetTargetPort())
		}
		targetHost = open.GetTargetHost()
		switch {
		case targetHost != "":
			if !isAdmin {
				return "", "", 0, targetType, targetID, desc, audit.RiskHigh, fmt.Errorf("仅管理员可指定任意目标地址")
			}
			risk = audit.RiskHigh
		case matched.Mesh:
			targetHost = s.deps.MeshIP(hostID)
			if targetHost == "" {
				return "", "", 0, targetType, targetID, desc, audit.RiskMedium,
					fmt.Errorf("该端口仅绑定异地组网，但主机当前无可用组网 IP")
			}
			risk = audit.RiskMedium
		default:
			targetHost = "127.0.0.1"
			risk = audit.RiskMedium
		}
	} else {
		if open.GetHostId() == "" {
			return "", "", 0, "", "", "", audit.RiskLow, fmt.Errorf("必须指定 host_id 或 app_id")
		}
		hostID = open.GetHostId()
		desc = "主机直连"
		targetHost = open.GetTargetHost()
		switch targetHost {
		case "", "127.0.0.1", "localhost":
			targetHost = "127.0.0.1"
			risk = audit.RiskMedium
		default:
			if !isAdmin {
				return "", "", 0, targetType, targetID, desc, audit.RiskHigh,
					fmt.Errorf("仅管理员可把隧道指向任意内网地址")
			}
			risk = audit.RiskHigh
		}
	}

	agent := s.deps.Registry.GetAgent(hostID)
	if agent == nil {
		return "", "", 0, targetType, targetID, desc, audit.RiskMedium, fmt.Errorf("目标不存在或无权访问")
	}
	if s.deps.CanAccessGroup != nil && !s.deps.CanAccessGroup(claims.Username, isAdmin, agent.Group) {
		// Same text as the not-found denials above: cross-group callers must
		// not learn that a host/app exists by comparing error messages.
		return "", "", 0, targetType, targetID, desc, audit.RiskMedium,
			fmt.Errorf("目标不存在或无权访问")
	}
	if hub := s.deps.Registry.Hub(hostID); hub == nil {
		return "", "", 0, targetType, targetID, desc, audit.RiskLow, fmt.Errorf("目标主机不在线")
	}
	return hostID, targetHost, targetPort, targetType, targetID, desc, risk, nil
}

// deny records a blocked open attempt and returns a gRPC status error.
func (s *OperatorService) deny(claims *auth.Claims, ip, targetType, targetID, reason, risk string) error {
	s.deps.Audit.Record(audit.Entry{
		Username:   claims.Username,
		Action:     "op_tunnel_deny",
		TargetType: targetType,
		TargetID:   targetID,
		Detail:     "拒绝运维隧道: " + reason,
		IP:         ip,
		RiskLevel:  risk,
		Result:     audit.ResultBlocked,
	})
	s.log.Warn("operator tunnel denied", "user", claims.Username, "reason", reason, "ip", ip)
	return status.Error(codes.PermissionDenied, reason)
}

// pumpClientToAgent forwards OperatorData frames to the agent tunnel leg.
func (s *OperatorService) pumpClientToAgent(stream grpc.BidiStreamingServer[agentpb.OperatorMessage, agentpb.OperatorMessage], hub *Hub, connID string, leg *tunnelConn, errCh chan<- error) {
	for {
		msg, err := stream.Recv()
		if err != nil {
			errCh <- fmt.Errorf("客户端断开")
			return
		}
		switch p := msg.Payload.(type) {
		case *agentpb.OperatorMessage_Data:
			if len(p.Data.GetData()) == 0 {
				continue
			}
			s.deps.Coordinator.TouchLeg(connID)
			if !hub.Send(&agentpb.ServerMessage{Payload: &agentpb.ServerMessage_TunnelData{
				TunnelData: &agentpb.TunnelData{TunnelId: connID, Data: p.Data.GetData()},
			}}) {
				errCh <- fmt.Errorf("Agent 通道不可写")
				return
			}
		case *agentpb.OperatorMessage_Close:
			errCh <- fmt.Errorf("客户端主动关闭: %s", p.Close.GetReason())
			return
		default:
			errCh <- fmt.Errorf("协议错误: 隧道建立后仅接受 data/close 帧")
			return
		}
	}
}

// pumpAgentToClient forwards agent payloads to the operator stream and is the
// sole sender after Connect's ack (done fires when the leg dies anywhere).
func (s *OperatorService) pumpAgentToClient(stream grpc.BidiStreamingServer[agentpb.OperatorMessage, agentpb.OperatorMessage], inbound <-chan []byte, leg *tunnelConn, errCh chan<- error) {
	for {
		select {
		case <-leg.done:
			_ = stream.Send(&agentpb.OperatorMessage{Payload: &agentpb.OperatorMessage_Close{
				Close: &agentpb.OperatorClose{Reason: "隧道腿已关闭"},
			}})
			errCh <- fmt.Errorf("Agent 侧隧道腿关闭")
			return
		case data := <-inbound:
			if len(data) == 0 {
				continue
			}
			if err := stream.Send(&agentpb.OperatorMessage{Payload: &agentpb.OperatorMessage_Data{
				Data: &agentpb.OperatorData{Data: data},
			}}); err != nil {
				errCh <- fmt.Errorf("客户端流写入失败")
				return
			}
		}
	}
}
