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

// Deployment target is configured via environment variables — never commit
// credentials to the repo.
//
//	WATCHMAN_DEPLOY_HOST  e.g. "186.241.120.46:22"
//	WATCHMAN_DEPLOY_USER  e.g. "root"
//	WATCHMAN_DEPLOY_PASS  the SSH password / key passphrase
func deployTarget() (host, user, pass string, err error) {
	host = os.Getenv("WATCHMAN_DEPLOY_HOST")
	user = os.Getenv("WATCHMAN_DEPLOY_USER")
	pass = os.Getenv("WATCHMAN_DEPLOY_PASS")
	if host == "" || user == "" || pass == "" {
		return "", "", "", fmt.Errorf("set WATCHMAN_DEPLOY_HOST, WATCHMAN_DEPLOY_USER and WATCHMAN_DEPLOY_PASS env vars")
	}
	return host, user, pass, nil
}

func getSSHClient() (*ssh.Client, error) {
	remoteHost, remoteUser, remotePass, err := deployTarget()
	if err != nil {
		return nil, err
	}
	config := &ssh.ClientConfig{
		User: remoteUser,
		Auth: []ssh.AuthMethod{
			ssh.Password(remotePass),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         15 * time.Second,
	}
	var lastErr error
	for i := 1; i <= 3; i++ {
		client, err := ssh.Dial("tcp", remoteHost, config)
		if err == nil {
			return client, nil
		}
		lastErr = err
		time.Sleep(2 * time.Second)
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
		cmd := fmt.Sprintf("mkdir -p %s && gzip -dc > %s && chmod %o %s", dir, remoteClean, mode, remoteClean)

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

func deployAll(client *ssh.Client) error {
	fmt.Println("==> 1. Preparing remote directory structure & stopping services...")
	_, _ = runCommand(client, "systemctl stop watchman-server watchman-agent || true")
	out, err := runCommand(client, "mkdir -p /opt/watchman/bin /opt/watchman/data /opt/watchman/web/dist && rm -f /opt/watchman/bin/watchman-server /opt/watchman/bin/watchman-agent")
	if err != nil {
		return fmt.Errorf("mkdir/rm: %v, out: %s", err, out)
	}

	fmt.Println("==> 2. Uploading watchman-server binary (35MB)...")
	if err := uploadFile("bin/linux_amd64/watchman-server", "/opt/watchman/bin/watchman-server", 0755); err != nil {
		return fmt.Errorf("upload watchman-server: %w", err)
	}
	fmt.Println("    watchman-server uploaded.")

		fmt.Println("==> 3. Uploading watchman-agent binaries...")
		if err := uploadFile("bin/linux_amd64/watchman-agent", "/opt/watchman/bin/watchman-agent", 0755); err != nil {
			return fmt.Errorf("upload watchman-agent: %w", err)
		}
		if err := uploadFile("bin/linux_amd64/watchman-agent-linux-amd64", "/opt/watchman/bin/watchman-agent-linux-amd64", 0755); err != nil {
			fmt.Printf("    warning: upload watchman-agent-linux-amd64: %v\n", err)
		}
		if err := uploadFile("bin/linux_amd64/watchman-agent-linux-arm64", "/opt/watchman/bin/watchman-agent-linux-arm64", 0755); err != nil {
			fmt.Printf("    warning: upload watchman-agent-linux-arm64: %v\n", err)
		}
		if err := uploadFile("bin/linux_amd64/watchman-agent-windows-amd64.exe", "/opt/watchman/bin/watchman-agent-windows-amd64.exe", 0755); err != nil {
			fmt.Printf("    warning: upload watchman-agent-windows-amd64.exe: %v\n", err)
		}
		fmt.Println("    watchman-agent binaries uploaded.")

	fmt.Println("==> 4. Uploading frontend web-dist.tar.gz...")
	if err := uploadFile("bin/linux_amd64/web-dist.tar.gz", "/opt/watchman/web/web-dist.tar.gz", 0644); err != nil {
		return fmt.Errorf("upload web-dist.tar.gz: %w", err)
	}
	fmt.Println("    Extracting web frontend assets...")
	out, err = runCommand(client, "tar -xzf /opt/watchman/web/web-dist.tar.gz -C /opt/watchman/web/dist && rm -f /opt/watchman/web/web-dist.tar.gz")
	if err != nil {
		return fmt.Errorf("tar extract: %v, out: %s", err, out)
	}
	fmt.Println("    Frontend assets extracted.")

	fmt.Println("==> 5. Installing Nginx if missing...")
	out, _ = runCommand(client, "which nginx || (DEBIAN_FRONTEND=noninteractive apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y nginx)")
	fmt.Printf("    Nginx status: %s\n", strings.TrimSpace(out))

	fmt.Println("==> 6. Writing Nginx reverse proxy configuration...")
	nginxConf := `server {
    listen 80 default_server;
    listen [::]:80 default_server;
    server_name _;

    root /opt/watchman/web/dist;
    index index.html;

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
    }

    location /install {
        proxy_pass http://127.0.0.1:18080/install;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
    }
}`
	writeNginxCmd := fmt.Sprintf("cat << 'EOF' > /etc/nginx/sites-available/watchman\n%s\nEOF\nln -sf /etc/nginx/sites-available/watchman /etc/nginx/sites-enabled/default\nnginx -t && systemctl reload nginx || systemctl restart nginx", nginxConf)
	out, err = runCommand(client, writeNginxCmd)
	if err != nil {
		return fmt.Errorf("nginx config: %v, out: %s", err, out)
	}
	fmt.Printf("    Nginx reloaded: %s\n", strings.TrimSpace(out))

	fmt.Println("==> 7. Configuring systemd services...")
	serverService := `[Unit]
Description=Watchman Control Server
After=network.target

[Service]
Type=simple
WorkingDirectory=/opt/watchman
ExecStart=/opt/watchman/bin/watchman-server -grpc :9090 -http :18080 -data /opt/watchman/data
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target`

	writeServerCmd := fmt.Sprintf("cat << 'EOF' > /etc/systemd/system/watchman-server.service\n%s\nEOF\nsystemctl daemon-reload\nsystemctl restart watchman-server\nsystemctl enable watchman-server", serverService)
	out, err = runCommand(client, writeServerCmd)
	if err != nil {
		return fmt.Errorf("systemd watchman-server: %v, out: %s", err, out)
	}
	fmt.Println("    watchman-server started and enabled.")

	fmt.Println("==> 8. Enrolling and starting watchman-agent...")
	time.Sleep(2 * time.Second)

	// Obtain enroll token via local curl to server
	loginCmd := `curl -s -X POST http://127.0.0.1:18080/api/v1/auth/login -H "Content-Type: application/json" -d '{"username":"admin","password":"admin"}'`
	out, err = runCommand(client, loginCmd)
	if err != nil {
		return fmt.Errorf("login failed: %v, out: %s", err, out)
	}
	var loginResp struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(out), &loginResp); err != nil || loginResp.Token == "" {
		fmt.Printf("    login output: %s\n", out)
	}

	enrollCmd := fmt.Sprintf(`curl -s -X POST http://127.0.0.1:18080/api/v1/hosts/enroll -H "Authorization: Bearer %s"`, loginResp.Token)
	out, err = runCommand(client, enrollCmd)
	var enrollResp struct {
		EnrollToken string `json:"enroll_token"`
	}
	_ = json.Unmarshal([]byte(out), &enrollResp)
	fmt.Printf("    Enroll token generated: %s\n", enrollResp.EnrollToken)

	// Remove old agent state to ensure clean enrollment
	_, _ = runCommand(client, "systemctl stop watchman-agent || true")
	_, _ = runCommand(client, "rm -f /opt/watchman/data/agent-state.json /var/lib/watchman-agent/state.json")

	agentService := fmt.Sprintf(`[Unit]
Description=Watchman Agent
After=network.target watchman-server.service

[Service]
Type=simple
WorkingDirectory=/opt/watchman
ExecStart=/opt/watchman/bin/watchman-agent -server 127.0.0.1:9090 -enroll %s -state /opt/watchman/data/agent-state.json
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target`, enrollResp.EnrollToken)

	writeAgentCmd := fmt.Sprintf("cat << 'EOF' > /etc/systemd/system/watchman-agent.service\n%s\nEOF\nsystemctl daemon-reload\nsystemctl restart watchman-agent\nsystemctl enable watchman-agent", agentService)
	out, err = runCommand(client, writeAgentCmd)
	if err != nil {
		return fmt.Errorf("systemd watchman-agent: %v, out: %s", err, out)
	}
	fmt.Println("    watchman-agent started and enabled.")

	time.Sleep(3 * time.Second)

	fmt.Println("==> 9. Verifying cluster state...")
	listHostsCmd := fmt.Sprintf(`curl -s -X GET http://127.0.0.1:18080/api/v1/hosts -H "Authorization: Bearer %s"`, loginResp.Token)
	out, _ = runCommand(client, listHostsCmd)
	fmt.Printf("    Online hosts in registry: %s\n", out)

	return nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: deploy <test|deploy|exec <cmd>|upload <local> <remote> [perm]>")
		return
	}

	client, err := getSSHClient()
	if err != nil {
		log.Fatalf("SSH connection failed: %v", err)
	}
	defer client.Close()

	switch os.Args[1] {
	case "test":
		out, err := runCommand(client, "uname -a && cat /etc/os-release && docker --version || true")
		if err != nil {
			log.Fatalf("Command error: %v, out: %s", err, out)
		}
		fmt.Printf("SSH Connected Successfully!\nRemote info:\n%s\n", out)

	case "deploy":
		if err := deployAll(client); err != nil {
			log.Fatalf("Deployment failed: %v", err)
		}
		fmt.Println("\n==========================================")
		fmt.Println("🎉 Watchman deployed successfully!")
		if h, _, _ := strings.Cut(os.Getenv("WATCHMAN_DEPLOY_HOST"), ":"); h != "" {
			fmt.Printf("Web Access: http://%s/\n", h)
		}
		fmt.Println("==========================================")

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

