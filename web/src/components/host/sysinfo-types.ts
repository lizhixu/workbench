export interface Process {
  pid: number
  name: string
  user: string
  cpu: number
  mem: number
  cmdline: string
}

export interface Port {
  proto: string
  address: string
  port: number
  state: string
  pid: number
  process: string
}

export interface FirewallRule {
  port: number
  protocol: string
  action: 'allow' | 'deny'
}

export interface FirewallStatus {
  supported: boolean
  backend: 'ufw' | 'firewalld' | 'iptables' | 'none'
  enabled: boolean
  rules: FirewallRule[]
  hint?: string
}