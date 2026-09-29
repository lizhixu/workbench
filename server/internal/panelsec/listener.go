package panelsec

import (
	"bytes"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"time"

	"watchman/server/internal/settings"
)

// DynamicListener wraps a net.Listener to inspect incoming TCP connections.
// If panel SSL is enabled and the connection begins with a TLS ClientHello
// record byte (0x16), it wraps the connection in tls.Server so http.Server
// serves it as HTTPS. Otherwise, it serves it as plaintext HTTP.
type DynamicListener struct {
	net.Listener
	tlsConfig *tls.Config
	settings  *settings.Store
	log       *slog.Logger
}

// NewDynamicListener builds a sniffing dynamic TLS listener.
func NewDynamicListener(inner net.Listener, tlsConfig *tls.Config, st *settings.Store, log *slog.Logger) *DynamicListener {
	if log == nil {
		log = slog.Default()
	}
	return &DynamicListener{
		Listener:  inner,
		tlsConfig: tlsConfig,
		settings:  st,
		log:       log,
	}
}

// prefixConn combines an already read byte slice with the remaining net.Conn stream.
type prefixConn struct {
	net.Conn
	r io.Reader
}

func (c *prefixConn) Read(p []byte) (int, error) {
	return c.r.Read(p)
}

// Accept waits for and returns the next connection to the listener.
func (l *DynamicListener) Accept() (net.Conn, error) {
	for {
		rawConn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}

		sslActive := false
		if l.settings != nil {
			sslActive = l.settings.PanelSecurity().SSLEnabled
		}
		if !sslActive && l.tlsConfig != nil && len(l.tlsConfig.Certificates) > 0 {
			sslActive = true
		}

		if !sslActive {
			return rawConn, nil
		}

		// Sniff the first byte within 1.5 seconds.
		_ = rawConn.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
		var b [1]byte
		n, err := rawConn.Read(b[:])
		_ = rawConn.SetReadDeadline(time.Time{})

		if err != nil || n == 0 {
			// Client disconnected, timed out, or port-scanned without data.
			_ = rawConn.Close()
			continue
		}

		pConn := &prefixConn{
			Conn: rawConn,
			r:    io.MultiReader(bytes.NewReader(b[:1]), rawConn),
		}

		if b[0] == 0x16 {
			// TLS ClientHello handshake packet.
			return tls.Server(pConn, l.tlsConfig), nil
		}

		// Plaintext HTTP request.
		return pConn, nil
	}
}
