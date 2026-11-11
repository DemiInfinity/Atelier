package runner

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"atelier/internal/actions"
)

const (
	logFlushEvery = 250 * time.Millisecond
	logBatchLines = 200
	maxLineLen    = 64 * 1024
)

// logStream batches masked log lines and ships them to the server. A reply
// with cancel=true calls onCancel once.
type logStream struct {
	client   *Client
	jobID    int64
	mask     *Masker
	onCancel func()
	log      *slog.Logger
	// tap, when set before the first line, also gets every masked line
	// (the control channel's tail keeps the last few).
	tap func(text string)

	mu      sync.Mutex
	pending []actions.LogLine
	kick    chan struct{}
	sendMu  sync.Mutex // one upload at a time, in order
	done    chan struct{}
	stopped chan struct{}
	once    sync.Once
}

func newLogStream(c *Client, jobID int64, m *Masker, onCancel func(), l *slog.Logger) *logStream {
	s := &logStream{client: c, jobID: jobID, mask: m, onCancel: onCancel, log: l,
		kick: make(chan struct{}, 1), done: make(chan struct{}), stopped: make(chan struct{})}
	go s.loop()
	return s
}

// Line appends one line (masked). Text containing newlines is split.
func (s *logStream) Line(step int, stream, text string) {
	text = strings.TrimRight(text, "\r\n")
	for _, part := range strings.Split(text, "\n") {
		part = strings.TrimSuffix(part, "\r")
		if len(part) > maxLineLen {
			part = part[:maxLineLen] + "…"
		}
		masked := s.mask.Mask(part)
		if s.tap != nil {
			s.tap(masked)
		}
		s.mu.Lock()
		s.pending = append(s.pending, actions.LogLine{Time: time.Now().UTC(), Step: step, Stream: stream, Text: masked})
		n := len(s.pending)
		s.mu.Unlock()
		if n >= logBatchLines {
			select {
			case s.kick <- struct{}{}:
			default:
			}
		}
	}
}

// System writes a runner message.
func (s *logStream) System(step int, text string) { s.Line(step, "system", text) }

func (s *logStream) loop() {
	defer close(s.stopped)
	t := time.NewTicker(logFlushEvery)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			s.Flush()
			return
		case <-t.C:
		case <-s.kick:
		}
		s.Flush()
	}
}

// Flush uploads everything pending, synchronously.
func (s *logStream) Flush() {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	for {
		s.mu.Lock()
		n := min(len(s.pending), logBatchLines*5)
		batch := s.pending[:n:n]
		s.pending = s.pending[n:]
		s.mu.Unlock()
		if len(batch) == 0 {
			return
		}
		var err error
		for attempt := range 4 {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			var resp *actions.LogsResponse
			resp, err = s.client.Logs(ctx, s.jobID, batch)
			cancel()
			if err == nil {
				if resp.Cancel && s.onCancel != nil {
					s.onCancel()
				}
				break
			}
			time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
		}
		if err != nil {
			s.log.Warn("dropping job log lines", "job", s.jobID, "lines", len(batch), "err", s.mask.Mask(err.Error()))
		}
	}
}

// Close flushes and stops the uploader.
func (s *logStream) Close() {
	s.once.Do(func() { close(s.done) })
	<-s.stopped
}
