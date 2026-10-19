// 异地组网（Tailscale）相关的共享判定。

// bothMeshed：链路两侧都持有组网 IP（100.x）时流量才走 Tailscale 组网，
// 否则回退为直连（公网/内网 IP）。网关模式与组网端口转发同用此判定。
export function bothMeshed(a?: string, b?: string): boolean {
  return !!a && !!b
}
