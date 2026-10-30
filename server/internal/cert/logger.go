package cert

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// maxCertLogBytes bounds in-memory and persisted log buffer to prevent
// uncontrolled growth (aligning with apps/deploy.go maxBuildLogBytes).
const maxCertLogBytes = 64 * 1024

// certLogger accumulates structured execution steps for an issuance, renewal,
// or verification flow. It writes simultaneously to an in-memory string buffer
// and structured slog output.
type certLogger struct {
	mu  sync.Mutex
	b   strings.Builder
	log *slog.Logger
	id  string
}

func newCertLogger(id string, baseLog *slog.Logger) *certLogger {
	if baseLog == nil {
		baseLog = slog.Default()
	}
	return &certLogger{
		log: baseLog,
		id:  id,
	}
}

// Log records a single timestamped stage entry.
func (l *certLogger) Log(stage, msg string) {
	nowStr := time.Now().Format("2006-01-02 15:04:05")
	entry := fmt.Sprintf("[%s] [%s] %s\n", nowStr, stage, msg)

	l.mu.Lock()
	if l.b.Len() < maxCertLogBytes {
		if l.b.Len()+len(entry) > maxCertLogBytes {
			l.b.WriteString("...[log truncated]...\n")
		} else {
			l.b.WriteString(entry)
		}
	}
	l.mu.Unlock()

	l.log.Info(msg, "stage", stage, "target", l.id)
}

// Logf records a formatted stage entry.
func (l *certLogger) Logf(stage, format string, args ...any) {
	l.Log(stage, fmt.Sprintf(format, args...))
}

// Warn records a warning stage entry to buffer and slog.Warn.
func (l *certLogger) Warn(stage, msg string) {
	nowStr := time.Now().Format("2006-01-02 15:04:05")
	entry := fmt.Sprintf("[%s] [WARN:%s] %s\n", nowStr, stage, msg)

	l.mu.Lock()
	if l.b.Len() < maxCertLogBytes {
		if l.b.Len()+len(entry) > maxCertLogBytes {
			l.b.WriteString("...[log truncated]...\n")
		} else {
			l.b.WriteString(entry)
		}
	}
	l.mu.Unlock()

	l.log.Warn(msg, "stage", stage, "target", l.id)
}

// Warnf records a formatted warning stage entry.
func (l *certLogger) Warnf(stage, format string, args ...any) {
	l.Warn(stage, fmt.Sprintf(format, args...))
}

// String returns the full accumulated log string.
func (l *certLogger) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
