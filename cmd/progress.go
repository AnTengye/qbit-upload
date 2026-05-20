package cmd

import (
	"strings"
	"sync"
)

type progressSnapshot struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func newProgressSnapshot(limit int) *progressSnapshot {
	if limit <= 0 {
		limit = 1024
	}
	return &progressSnapshot{limit: limit}
}

func (p *progressSnapshot) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.buf = append(p.buf, data...)
	if len(p.buf) > p.limit {
		p.buf = append([]byte(nil), p.buf[len(p.buf)-p.limit:]...)
	}
	return len(data), nil
}

func (p *progressSnapshot) Latest() string {
	p.mu.Lock()
	defer p.mu.Unlock()

	latest := strings.TrimSpace(string(p.buf))
	if latest == "" {
		return "暂无输出"
	}
	return latest
}
