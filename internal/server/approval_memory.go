package server

import "sync"

// approvalMemory implements "users may remember the choice per
// conversation" (§6.1 step 5, §9.9 layer 6). Deliberately in-memory only:
// a server restart re-opens every consent prompt (a restart is a fresh
// consent window), and entries drop with the conversation. Remembering is
// offered for code_run only — the other confirm-grade tools stay gated.
type approvalMemory struct {
	mu     sync.Mutex
	byConv map[string]map[string]bool // convID → tool → approved
}

func newApprovalMemory() *approvalMemory {
	return &approvalMemory{byConv: map[string]map[string]bool{}}
}

// bounded: a long-lived server must not grow one entry per conversation
// forever; past the cap new conversations simply never remember.
const approvalMemoryCap = 4096

func (m *approvalMemory) remember(convID, tool string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byConv[convID] == nil && len(m.byConv) >= approvalMemoryCap {
		return
	}
	if m.byConv[convID] == nil {
		m.byConv[convID] = map[string]bool{}
	}
	m.byConv[convID][tool] = true
}

func (m *approvalMemory) remembered(convID, tool string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byConv[convID][tool]
}

func (m *approvalMemory) forget(convID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.byConv, convID)
}
