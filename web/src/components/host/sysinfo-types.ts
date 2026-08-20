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