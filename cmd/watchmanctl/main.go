// watchmanctl is the operator CLI for watchman's on-demand tunnels
// (kubectl port-forward style):
//
//	watchmanctl login --server https://panel.example.com:18789
//	watchmanctl tunnel --app <appID> --port 8080 [--listen [ip:]port]
//	watchmanctl tunnel --host <hostID|hostname> --target [host:]port [--listen ...]
//	watchmanctl ls
//	watchmanctl close <connID>
//	watchmanctl version
//
// A tunnel listens on a loopback port of the operator's machine and proxies
// each local connection through the control plane's dedicated operator data
// plane (-opgrpc, TLS) to a port on a managed host. Nothing is opened on any
// host, and no business bytes touch the web panel port.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"watchman/internal/version"
)

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

func realMain(args []string, out, errOut io.Writer) int {
	if len(args) == 0 {
		usage(errOut)
		return 2
	}
	var code int
	switch args[0] {
	case "login":
		code = cmdLogin(args[1:], out, errOut)
	case "tunnel":
		code = cmdTunnel(args[1:], out, errOut)
	case "ls":
		code = cmdLs(args[1:], out, errOut)
	case "close":
		code = cmdClose(args[1:], out, errOut)
	case "version", "--version", "-version":
		fmt.Fprintf(out, "watchmanctl %s (commit %s, built %s)\n", version.Get(), version.Commit, version.BuildTime)
		return 0
	case "help", "-h", "--help":
		usage(out)
		return 0
	default:
		fmt.Fprintf(errOut, "unknown command %q\n\n", args[0])
		usage(errOut)
		return 2
	}
	return code
}

func usage(w io.Writer) {
	fmt.Fprint(w, `watchmanctl - on-demand ops tunnels for watchman (kubectl port-forward style)

Usage:
  watchmanctl login --server <https://panel:18789> [--insecure] [--username u]
  watchmanctl tunnel --app <appID> --port <n> [--listen [ip:]port]
  watchmanctl tunnel --host <hostID|hostname> --target [host:]port [--listen [ip:]port]
  watchmanctl tunnel --host <hostID|hostname> --port <n> [--listen [ip:]port]   (loopback target)
  watchmanctl ls
  watchmanctl close <connID>
  watchmanctl version

A tunnel binds a loopback port on this machine; every local connection is
proxied to the target through the control plane's operator data plane (TLS).
No port is opened on the managed host and tunnels never survive this process.

Global flags for tunnel/ls/close: --server and --insecure override the values
saved by login (not persisted).
`)
}

// globalFlags adds the override flags shared by tunnel/ls/close.
type globalFlags struct {
	server   *string
	insecure *bool
}

func addGlobalFlags(fs *flag.FlagSet) globalFlags {
	return globalFlags{
		server:   fs.String("server", "", "panel base URL (overrides the one saved by login)"),
		insecure: fs.Bool("insecure", false, "skip TLS verification (overrides the saved setting)"),
	}
}

// loadEffectiveConfig loads the saved config and applies flag overrides.
func loadEffectiveConfig(g globalFlags) (*config, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	if *g.server != "" {
		cfg.Server = *g.server
	}
	if *g.insecure {
		cfg.Insecure = true
	}
	if cfg.Server == "" || cfg.Token == "" {
		return nil, fmt.Errorf("not logged in; run `watchmanctl login --server <url>` first")
	}
	return cfg, nil
}

func warnInsecure(cfg *config, errOut io.Writer) {
	if cfg.Insecure {
		fmt.Fprintln(errOut, "warning: TLS verification is disabled (insecure mode), the connection is not authenticated")
	}
}

func cmdLogin(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(errOut)
	server := fs.String("server", "", "panel base URL (required), e.g. https://panel.example.com:18789")
	insecure := fs.Bool("insecure", false, "skip TLS verification (self-signed panels); persisted")
	username := fs.String("username", "", "login username (prompted when omitted)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *server == "" {
		fmt.Fprintln(errOut, "--server is required")
		return 2
	}

	// A single bufio.Reader for the whole prompt sequence: two readers over the
	// same os.Stdin would split piped credentials ("printf 'u\np\n' | ...")
	// when the first reader's read-ahead buffer swallows the password line.
	in := bufio.NewReader(os.Stdin)
	u := *username
	if u == "" {
		fmt.Fprint(out, "Username: ")
		line, err := in.ReadString('\n')
		if err != nil {
			fmt.Fprintf(errOut, "read username: %v\n", err)
			return 1
		}
		u = strings.TrimSpace(line)
	}
	if u == "" {
		fmt.Fprintln(errOut, "username must not be empty")
		return 1
	}

	var password string
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(out, "Password: ")
		pw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(out)
		if err != nil {
			fmt.Fprintf(errOut, "read password: %v\n", err)
			return 1
		}
		password = string(pw)
	} else {
		line, err := in.ReadString('\n')
		if err != nil {
			fmt.Fprintf(errOut, "read password: %v\n", err)
			return 1
		}
		password = strings.TrimRight(line, "\r\n")
	}

	cfg := &config{Server: strings.TrimSuffix(*server, "/"), Insecure: *insecure}
	warnInsecure(cfg, errOut)
	token, role, err := newAPIClient(cfg).login(u, password)
	if err != nil {
		fmt.Fprintf(errOut, "login failed: %v\n", err)
		return 1
	}
	cfg.Token = token
	if err := cfg.save(); err != nil {
		fmt.Fprintf(errOut, "save config: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "Logged in to %s as %s (%s)\n", cfg.Server, u, role)
	return 0
}

// parseTarget parses "[host:]port" into its two parts. A bare port means the
// server picks the target host (loopback / mesh IP resolution).
func parseTarget(s string) (string, uint32, error) {
	if s == "" {
		return "", 0, fmt.Errorf("empty target")
	}
	if p, err := strconv.Atoi(s); err == nil {
		if p < 1 || p > 65535 {
			return "", 0, fmt.Errorf("port out of range: %d", p)
		}
		return "", uint32(p), nil
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return "", 0, fmt.Errorf("target must be [host:]port: %v", err)
	}
	if host == "" {
		return "", 0, fmt.Errorf("empty target host; use a bare port for the default (loopback/mesh) target")
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return "", 0, fmt.Errorf("invalid port %q", port)
	}
	return host, uint32(p), nil
}

func cmdTunnel(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("tunnel", flag.ContinueOnError)
	fs.SetOutput(errOut)
	g := addGlobalFlags(fs)
	appID := fs.String("app", "", "application id (app mode: port allowlist enforced)")
	hostRef := fs.String("host", "", "host id or exact hostname (host mode)")
	target := fs.String("target", "", "target [host:]port (host mode; bare port = loopback/mesh)")
	port := fs.Int("port", 0, "target port (shortcut app mode / loopback host mode)")
	listen := fs.String("listen", "", "local bind [ip:]port (default: random loopback port)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	opt := tunnelOptions{Listen: *listen}
	switch {
	case *appID != "" && *hostRef != "":
		fmt.Fprintln(errOut, "--app and --host are mutually exclusive")
		return 2
	case *appID == "" && *hostRef == "":
		fmt.Fprintln(errOut, "either --app or --host is required")
		return 2
	case *target != "" && *port != 0:
		fmt.Fprintln(errOut, "--target and --port are mutually exclusive")
		return 2
	}

	if *appID != "" {
		if *target != "" {
			fmt.Fprintln(errOut, "app mode takes --port, not --target (the server resolves the host)")
			return 2
		}
		if *port < 1 || *port > 65535 {
			fmt.Fprintln(errOut, "app mode requires --port 1..65535")
			return 2
		}
		opt.AppID = *appID
		opt.TargetPort = uint32(*port)
	} else {
		if *target != "" {
			h, p, err := parseTarget(*target)
			if err != nil {
				fmt.Fprintf(errOut, "%v\n", err)
				return 2
			}
			opt.TargetHost, opt.TargetPort = h, p
		} else {
			if *port < 1 || *port > 65535 {
				fmt.Fprintln(errOut, "host mode requires --target [host:]port or --port 1..65535")
				return 2
			}
			opt.TargetPort = uint32(*port)
		}
	}

	cfg, err := loadEffectiveConfig(g)
	if err != nil {
		fmt.Fprintf(errOut, "%v\n", err)
		return 1
	}
	warnInsecure(cfg, errOut)
	api := newAPIClient(cfg)

	if *hostRef != "" {
		h, err := api.resolveHost(*hostRef)
		if err != nil {
			fmt.Fprintf(errOut, "resolve host: %v\n", err)
			return 1
		}
		opt.HostID = h.ID
	}

	// Fetch the data-plane endpoint fresh on every tunnel start so panel moves
	// never strand operators behind a stale cached address.
	info, err := api.clientInfo()
	if err != nil {
		fmt.Fprintf(errOut, "query tunnel service: %v\n", err)
		return 1
	}
	if !info.Enabled || info.Endpoint == "" {
		fmt.Fprintln(errOut, "the panel's operator tunnel port (-opgrpc) is disabled; ask an administrator to enable it")
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	laddr, err := normalizeListen(opt.Listen)
	if err != nil {
		fmt.Fprintf(errOut, "%v\n", err)
		return 2
	}
	lis, err := net.Listen("tcp", laddr)
	if err != nil {
		fmt.Fprintf(errOut, "listen %s: %v\n", laddr, err)
		return 1
	}

	if err := runTunnel(ctx, lis, cfg, opt, info, dialOperator, out); err != nil {
		fmt.Fprintf(errOut, "tunnel: %v\n", err)
		return 1
	}
	fmt.Fprintln(out, "Tunnel closed.")
	return 0
}

func cmdLs(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	fs.SetOutput(errOut)
	g := addGlobalFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadEffectiveConfig(g)
	if err != nil {
		fmt.Fprintf(errOut, "%v\n", err)
		return 1
	}
	warnInsecure(cfg, errOut)
	tunnels, err := newAPIClient(cfg).listMyTunnels()
	if err != nil {
		fmt.Fprintf(errOut, "list tunnels: %v\n", err)
		return 1
	}
	if len(tunnels) == 0 {
		fmt.Fprintln(out, "No active tunnels.")
		return 0
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "CONN ID\tHOST\tTARGET\tIDLE\tAGE")
	for _, t := range tunnels {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			t.ConnID, t.AgentID, t.Target,
			(time.Duration(t.IdleSeconds) * time.Second).Round(time.Second),
			time.Since(t.CreatedAt).Round(time.Second))
	}
	_ = tw.Flush()
	return 0
}

func cmdClose(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("close", flag.ContinueOnError)
	fs.SetOutput(errOut)
	g := addGlobalFlags(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(errOut, "usage: watchmanctl close <connID>")
		return 2
	}
	cfg, err := loadEffectiveConfig(g)
	if err != nil {
		fmt.Fprintf(errOut, "%v\n", err)
		return 1
	}
	warnInsecure(cfg, errOut)
	id := fs.Arg(0)
	if err := newAPIClient(cfg).closeMyTunnel(id); err != nil {
		fmt.Fprintf(errOut, "close tunnel: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "Closed %s\n", id)
	return 0
}
