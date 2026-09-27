package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Target host credentials come from the environment so they are never committed.
//
//	WATCHMAN_DEPLOY_HOST  host or host:port (default port 22)
//	WATCHMAN_DEPLOY_USER  ssh user (default root)
//	WATCHMAN_DEPLOY_PASS  ssh password
var (
	remoteHost = envOr("WATCHMAN_DEPLOY_HOST", "")
	remoteUser = envOr("WATCHMAN_DEPLOY_USER", "root")
	remotePass = os.Getenv("WATCHMAN_DEPLOY_PASS")
)

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// remoteAddr returns host:port, defaulting the port to 22.
func remoteAddr() string {
	h := remoteHost
	if !strings.Contains(h, ":") {
		h += ":22"
	}
	return h
}

// publicHost strips the port for use in user-facing URLs.
func publicHost() string {
	h := remoteHost
	if i := strings.Index(h, ":"); i >= 0 {
		h = h[:i]
	}
	return h
}

func getSSHClient() (*ssh.Client, error) {
	if remoteHost == "" || remotePass == "" {
		return nil, fmt.Errorf("WATCHMAN_DEPLOY_HOST and WATCHMAN_DEPLOY_PASS must be set")
	}
	config := &ssh.ClientConfig{
		User: remoteUser,
		Auth: []ssh.AuthMethod{
			ssh.Password(remotePass),
			// Some sshd configs only offer keyboard-interactive for passwords.
			ssh.KeyboardInteractive(func(user, instruction string, questions []string, echos []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range answers {
					answers[i] = remotePass
				}
				return answers, nil
			}),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         45 * time.Second,
	}
	var lastErr error
	for i := 1; i <= 6; i++ {
		client, err := ssh.Dial("tcp", remoteAddr(), config)
		if err == nil {
			return client, nil
		}
		lastErr = err
		time.Sleep(time.Duration(i*2) * time.Second)
	}
	return nil, lastErr
}

func runCommand(client *ssh.Client, cmd string) (string, error) {
	session, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	err = session.Run(cmd)
	out := stdout.String()
	if stderr.Len() > 0 {
		out += "\nSTDERR: " + stderr.String()
	}
	return out, err
}

// runRetry executes a remote command on a freshly dialed connection, retrying
// transport failures. The link to the test server drops connections
// intermittently, so a long deploy cannot rely on one persistent client.
func runRetry(cmd string) (string, error) {
	var lastErr error
	for attempt := 1; attempt <= 4; attempt++ {
		if attempt > 1 {
			time.Sleep(time.Duration(attempt*3) * time.Second)
		}
		client, err := getSSHClient()
		if err != nil {
			lastErr = err
			continue
		}
		out, err := runCommand(client, cmd)
		client.Close()
		if err == nil {
			return out, nil
		}
		// A non-zero exit status is the command's own verdict, not a transport
		// fault; retrying it would only repeat the same failure.
		if _, ok := err.(*ssh.ExitError); ok {
			return out, err
		}
		lastErr = fmt.Errorf("%v, out: %s", err, out)
	}
	return "", lastErr
}

func uploadFile(localPath, remotePath string, mode os.FileMode) error {
	var lastErr error
	for attempt := 1; attempt <= 5; attempt++ {
		if attempt > 1 {
			fmt.Printf("    retry attempt %d/5 for %s...\n", attempt, localPath)
			time.Sleep(3 * time.Second)
		}

		client, err := getSSHClient()
		if err != nil {
			lastErr = fmt.Errorf("ssh dial: %w", err)
			continue
		}

		session, err := client.NewSession()
		if err != nil {
			client.Close()
			lastErr = fmt.Errorf("new ssh session: %w", err)
			continue
		}

		remoteClean := strings.ReplaceAll(remotePath, "\\", "/")
		dir := remoteClean[:strings.LastIndex(remoteClean, "/")]
		tmp := fmt.Sprintf("%s.tmp.%d", remoteClean, time.Now().UnixNano())
		cmd := fmt.Sprintf("mkdir -p %q && gzip -dc > %q && chmod %o %q && mv -f %q %q", dir, tmp, mode, tmp, tmp, remoteClean)

		stdin, err := session.StdinPipe()
		if err != nil {
			session.Close()
			client.Close()
			lastErr = fmt.Errorf("stdin pipe: %w", err)
			continue
		}

		var stderr bytes.Buffer
		session.Stderr = &stderr

		if err := session.Start(cmd); err != nil {
			stdin.Close()
			session.Close()
			client.Close()
			lastErr = fmt.Errorf("start remote cmd: %w", err)
			continue
		}

		src, err := os.Open(localPath)
		if err != nil {
			stdin.Close()
			session.Close()
			client.Close()
			return fmt.Errorf("open local file: %w", err)
		}

		gzWriter, _ := gzip.NewWriterLevel(stdin, gzip.BestSpeed)
		buf := make([]byte, 32*1024)
		n, err := io.CopyBuffer(gzWriter, src, buf)
		src.Close()
		if err != nil {
			gzWriter.Close()
			stdin.Close()
			session.Close()
			client.Close()
			lastErr = fmt.Errorf("gzip copy: %w, stderr: %s", err, stderr.String())
			continue
		}

		if err := gzWriter.Close(); err != nil {
			stdin.Close()
			session.Close()
			client.Close()
			lastErr = fmt.Errorf("gzip close: %w", err)
			continue
		}

		if err := stdin.Close(); err != nil {
			session.Close()
			client.Close()
			lastErr = fmt.Errorf("stdin close: %w", err)
			continue
		}

		if err := session.Wait(); err != nil {
			session.Close()
			client.Close()
			lastErr = fmt.Errorf("remote wait: %v, stderr: %s", err, stderr.String())
			continue
		}
		session.Close()
		client.Close()

		fmt.Printf("    transferred %d bytes (compressed) -> %s\n", n, remoteClean)
		return nil
	}
	return lastErr
}

func deployAll() error {
	fmt.Println("==> 1. Preparing remote directories & stopping services...")
	_, _ = runRetry("systemctl stop watchman-server watchman-agent 2>/dev/null || true")
	if _, err := runRetry("mkdir -p /opt/watchman/bin /opt/watchman/data /opt/watchman/web/dist && rm -f /opt/watchman/bin/watchman-server /opt/watchman/bin/watchman-agent"); err != nil {
		return fmt.Errorf("prepare dirs: %w", err)
	}

	fmt.Println("==> 2. Uploading watchman-server binary...")
	if err := uploadFile("bin/linux_amd64/watchman-server", "/opt/watchman/bin/watchman-server", 0755); err != nil {
		return fmt.Errorf("upload watchman-server: %w", err)
	}

	fmt.Println("==> 3. Uploading watchman-agent binaries...")
	if err := uploadFile("bin/linux_amd64/watchman-agent", "/opt/watchman/bin/watchman-agent", 0755); err != nil {
		return fmt.Errorf("upload watchman-agent: %w", err)
	}
	for _, f := range []string{
		"watchman-agent-linux-amd64",
		"watchman-agent-linux-arm64",
		"watchman-agent-windows-amd64.exe",
	} {
		if err := uploadFile("bin/linux_amd64/"+f, "/opt/watchman/bin/"+f, 0755); err != nil {
			fmt.Printf("    warning: upload %s: %v\n", f, err)
		}
	}

	fmt.Println("==> 4. Uploading and extracting frontend bundle...")
	if err := uploadFile("bin/linux_amd64/web-dist.tar.gz", "/opt/watchman/web/web-dist.tar.gz", 0644); err != nil {
		return fmt.Errorf("upload web-dist.tar.gz: %w", err)
	}
	if _, err := runRetry("rm -rf /opt/watchman/web/dist && mkdir -p /opt/watchman/web/dist && tar -xzf /opt/watchman/web/web-dist.tar.gz -C /opt/watchman/web/dist && rm -f /opt/watchman/web/web-dist.tar.gz && ls /opt/watchman/web/dist"); err != nil {
		return fmt.Errorf("extract frontend: %w", err)
	}

	fmt.Println("==> 5. Installing Nginx if missing...")
	out, err := runRetry("command -v nginx || (DEBIAN_FRONTEND=noninteractive apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq nginx >/dev/null && command -v nginx)")
	if err != nil {
		return fmt.Errorf("install nginx: %v, out: %s", err, out)
	}
	fmt.Printf("    nginx: %s\n", strings.TrimSpace(out))

	if err := configureNginx(); err != nil {
		return err
	}
	if err := configureServices(); err != nil {
		return err
	}
	return verify()
}

func configureNginx() error {
	fmt.Println("==> 6. Writing Nginx reverse proxy configuration...")
	nginxConf := `server {
    listen 80 default_server;
    listen [::]:80 default_server;
    server_name _;

    root /opt/watchman/web/dist;
    index index.html;

    # File-manager uploads stream through this proxy; nginx's 1m default
    # rejects ordinary files with 413 before the control server sees them.
    client_max_body_size 512m;

    # Debian ships "gzip on" with gzip_types commented out, which leaves the
    # built-in default of text/html only — every JS/CSS chunk then goes over the
    # wire uncompressed. The bundles are minified JS and compress ~70%, which is
    # the difference between a 1MB and a 340KB route chunk on a slow link.
    gzip on;
    gzip_vary on;
    gzip_comp_level 6;
    gzip_min_length 1024;
    gzip_proxied any;
    gzip_types
        text/plain
        text/css
        text/javascript
        application/javascript
        application/json
        application/wasm
        image/svg+xml;

    # Vite emits content-hashed filenames, so a new build produces new names and
    # these can be cached hard. Without this the browser revalidates every chunk
    # on each navigation and pays a round trip per file even on a 304.
    location /assets/ {
        expires 1y;
        add_header Cache-Control "public, immutable";
        access_log off;
        try_files $uri =404;
    }

    # index.html carries the asset references, so it must never be cached or a
    # client would keep loading the previous build's chunks.
    location = /index.html {
        add_header Cache-Control "no-cache, must-revalidate";
    }

    location / {
        try_files $uri $uri/ /index.html;
    }

    location /api/v1/ws/ {
        proxy_pass http://127.0.0.1:18080/api/v1/ws/;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_read_timeout 86400s;
        proxy_send_timeout 86400s;
    }

    location /api/v1/ {
        proxy_pass http://127.0.0.1:18080/api/v1/;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_read_timeout 600s;
        proxy_send_timeout 600s;
    }

    # Agent one-line installer and binary download (used by managed hosts).
    location /install {
        proxy_pass http://127.0.0.1:18080/install;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
    }

    location /install_script {
        proxy_pass http://127.0.0.1:18080/install_script;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
    }

    location /agent/ {
        proxy_pass http://127.0.0.1:18080/agent/;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_buffering off;
    }
}`
	writeNginxCmd := fmt.Sprintf("cat << 'WMEOF' > /etc/nginx/sites-available/watchman\n%s\nWMEOF\nrm -f /etc/nginx/sites-enabled/default\nln -sf /etc/nginx/sites-available/watchman /etc/nginx/sites-enabled/watchman\nnginx -t 2>&1 && (systemctl reload nginx || systemctl restart nginx) && systemctl enable nginx >/dev/null 2>&1; systemctl is-active nginx", nginxConf)
	out, err := runRetry(writeNginxCmd)
	if err != nil {
		return fmt.Errorf("nginx config: %v, out: %s", err, out)
	}
	fmt.Printf("    nginx: %s\n", strings.TrimSpace(out))
	return nil
}

func configureServices() error {
	fmt.Println("==> 7. Configuring systemd services...")
	// Signing/encryption secrets must survive restarts: a fresh JWT key would
	// invalidate every issued token, and a fresh vault passphrase would make
	// stored host credentials undecryptable. Generate once, then reuse.
	secretsCmd := `set -e
ENV=/opt/watchman/watchman.env
if [ ! -s "$ENV" ]; then
  umask 077
  printf 'WATCHMAN_JWT_KEY=%s\nWATCHMAN_VAULT_PASS=%s\n' "$(openssl rand -hex 32)" "$(openssl rand -hex 32)" > "$ENV"
  echo generated
else
  echo reused
fi`
	out, err := runRetry(secretsCmd)
	if err != nil {
		return fmt.Errorf("provision secrets: %v, out: %s", err, out)
	}
	fmt.Printf("    server secrets: %s\n", strings.TrimSpace(out))

	serverService := fmt.Sprintf(`[Unit]
Description=Watchman Control Server
After=network.target

[Service]
Type=simple
WorkingDirectory=/opt/watchman
EnvironmentFile=/opt/watchman/watchman.env
ExecStart=/opt/watchman/bin/watchman-server -grpc :9090 -http 127.0.0.1:18080 -data /opt/watchman/data -jwt-key ${WATCHMAN_JWT_KEY} -vault-pass ${WATCHMAN_VAULT_PASS} -ws-origins %s
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target`, publicHost())

	writeServerCmd := fmt.Sprintf("cat << 'WMEOF' > /etc/systemd/system/watchman-server.service\n%s\nWMEOF\nsystemctl daemon-reload && systemctl enable watchman-server >/dev/null 2>&1; systemctl restart watchman-server && sleep 3 && systemctl is-active watchman-server", serverService)
	out, err = runRetry(writeServerCmd)
	if err != nil {
		logs, _ := runRetry("journalctl -u watchman-server -n 30 --no-pager 2>&1 | tail -30")
		return fmt.Errorf("systemd watchman-server: %v, out: %s, logs:\n%s", err, out, logs)
	}
	fmt.Printf("    watchman-server: %s\n", strings.TrimSpace(out))

	fmt.Println("==> 8. Configuring and starting watchman-agent (self-managed host)...")
	loginToken, err := apiToken()
	if err != nil {
		return err
	}

	// Reuse an existing enrollment; a second -enroll would register a duplicate.
	stateCheck, _ := runRetry(`test -s /opt/watchman/data/agent-state.json && echo exists || echo none`)
	var enrollParam string
	if !strings.Contains(stateCheck, "exists") {
		out, err = runRetry(fmt.Sprintf(`curl -sS -X POST http://127.0.0.1:18080/api/v1/hosts/enroll -H "Authorization: Bearer %s"`, loginToken))
		if err != nil {
			return fmt.Errorf("issue enroll token: %v, out: %s", err, out)
		}
		var enrollResp struct {
			EnrollToken string `json:"enroll_token"`
			Token       string `json:"token"`
		}
		_ = json.Unmarshal([]byte(out), &enrollResp)
		tok := enrollResp.EnrollToken
		if tok == "" {
			tok = enrollResp.Token
		}
		if tok == "" {
			return fmt.Errorf("no enroll token in response: %s", out)
		}
		fmt.Printf("    enroll token issued: %s\n", tok)
		enrollParam = fmt.Sprintf("-enroll %s ", tok)
	} else {
		fmt.Println("    reusing existing agent enrollment state.")
	}

	agentService := fmt.Sprintf(`[Unit]
Description=Watchman Agent
After=network.target watchman-server.service

[Service]
Type=simple
WorkingDirectory=/opt/watchman
ExecStart=/opt/watchman/bin/watchman-agent -server 127.0.0.1:9090 %s-state /opt/watchman/data/agent-state.json
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target`, enrollParam)

	writeAgentCmd := fmt.Sprintf("cat << 'WMEOF' > /etc/systemd/system/watchman-agent.service\n%s\nWMEOF\nsystemctl daemon-reload && systemctl enable watchman-agent >/dev/null 2>&1; systemctl restart watchman-agent && sleep 3 && systemctl is-active watchman-agent", agentService)
	out, err = runRetry(writeAgentCmd)
	if err != nil {
		logs, _ := runRetry("journalctl -u watchman-agent -n 30 --no-pager 2>&1 | tail -30")
		return fmt.Errorf("systemd watchman-agent: %v, out: %s, logs:\n%s", err, out, logs)
	}
	fmt.Printf("    watchman-agent: %s\n", strings.TrimSpace(out))
	return nil
}

// apiToken logs in as the bootstrap admin and returns a bearer token.
func apiToken() (string, error) {
	loginCmd := `curl -sS -X POST http://127.0.0.1:18080/api/v1/auth/login -H "Content-Type: application/json" -d '{"username":"admin","password":"admin"}'`
	var lastOut string
	for attempt := 1; attempt <= 5; attempt++ {
		out, err := runRetry(loginCmd)
		lastOut = out
		if err == nil {
			var resp struct {
				Token string `json:"token"`
			}
			if json.Unmarshal([]byte(out), &resp) == nil && resp.Token != "" {
				return resp.Token, nil
			}
		}
		time.Sleep(3 * time.Second)
	}
	return "", fmt.Errorf("login failed, last response: %s", lastOut)
}

func verify() error {
	fmt.Println("==> 9. Verifying deployment...")
	token, err := apiToken()
	if err != nil {
		return err
	}
	fmt.Println("    admin login: ok")

	// The agent needs a moment to register after restart.
	var hosts string
	for attempt := 1; attempt <= 6; attempt++ {
		hosts, _ = runRetry(fmt.Sprintf(`curl -sS http://127.0.0.1:18080/api/v1/hosts -H "Authorization: Bearer %s"`, token))
		if strings.Contains(hosts, `"online"`) {
			break
		}
		time.Sleep(5 * time.Second)
	}
	fmt.Printf("    hosts: %s\n", strings.TrimSpace(hosts))

	out, _ := runRetry(`echo -n "index.html via nginx: "; curl -sS -o /dev/null -w "%{http_code}\n" http://127.0.0.1/; echo -n "api via nginx: "; curl -sS -o /dev/null -w "%{http_code}\n" -X POST http://127.0.0.1/api/v1/auth/login -H "Content-Type: application/json" -d '{"username":"admin","password":"admin"}'; echo -n "install script: "; curl -sS -o /dev/null -w "%{http_code}\n" http://127.0.0.1/install`)
	fmt.Printf("%s\n", strings.TrimSpace(out))

	svc, _ := runRetry("systemctl is-active watchman-server watchman-agent nginx | tr '\\n' ' '")
	fmt.Printf("    services (server agent nginx): %s\n", strings.TrimSpace(svc))
	return nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: deploy <test|deploy|nginx|verify|exec <cmd>|upload <local> <remote> [perm]|download <remote> <local>>")
		return
	}

	client, err := getSSHClient()
	if err != nil {
		log.Fatalf("SSH connection failed: %v", err)
	}
	defer client.Close()

	switch os.Args[1] {
	case "test":
		out, err := runCommand(client, "uname -a && cat /etc/os-release")
		if err != nil {
			log.Fatalf("Command error: %v, out: %s", err, out)
		}
		fmt.Printf("SSH Connected Successfully!\nRemote info:\n%s\n", out)

	case "deploy":
		client.Close() // deployAll dials per step so a dropped link can retry
		if err := deployAll(); err != nil {
			log.Fatalf("Deployment failed: %v", err)
		}
		fmt.Println("\n==========================================")
		fmt.Println("Watchman deployed successfully!")
		fmt.Printf("Web Access: http://%s/\n", publicHost())
		fmt.Println("Default Login: admin / admin")
		fmt.Println("==========================================")

	case "nginx":
		client.Close() // configureNginx dials per step
		if err := configureNginx(); err != nil {
			log.Fatalf("Nginx configuration failed: %v", err)
		}

	case "verify":
		client.Close()
		if err := verify(); err != nil {
			log.Fatalf("Verification failed: %v", err)
		}

	case "exec":
		if len(os.Args) < 3 {
			log.Fatal("missing cmd")
		}
		out, err := runCommand(client, os.Args[2])
		fmt.Println(out)
		if err != nil {
			log.Fatalf("exec error: %v", err)
		}

	case "download":
		if len(os.Args) < 4 {
			log.Fatal("missing args: download <remote> <local>")
		}
		remote := os.Args[2]
		local := os.Args[3]
		session, err := client.NewSession()
		if err != nil {
			log.Fatalf("new session: %v", err)
		}
		stdout, err := session.StdoutPipe()
		if err != nil {
			log.Fatalf("stdout pipe: %v", err)
		}
		if err := session.Start("gzip -c " + remote); err != nil {
			log.Fatalf("start gzip: %v", err)
		}
		outFile, err := os.Create(local)
		if err != nil {
			log.Fatalf("create local: %v", err)
		}
		gzReader, err := gzip.NewReader(stdout)
		if err != nil {
			log.Fatalf("gzip reader: %v", err)
		}
		n, err := io.Copy(outFile, gzReader)
		outFile.Close()
		gzReader.Close()
		_ = session.Wait()
		session.Close()
		if err != nil {
			log.Fatalf("download copy: %v", err)
		}
		fmt.Printf("Downloaded %s to %s (%d bytes)\n", remote, local, n)

	case "upload":
		if len(os.Args) < 4 {
			log.Fatal("missing args: upload <local> <remote>")
		}
		local := os.Args[2]
		remote := os.Args[3]
		mode := os.FileMode(0644)
		if len(os.Args) >= 5 && os.Args[4] == "755" {
			mode = os.FileMode(0755)
		}
		err := uploadFile(local, remote, mode)
		if err != nil {
			log.Fatalf("Upload failed: %v", err)
		}
		fmt.Printf("Uploaded %s to %s\n", local, remote)
	}
}
