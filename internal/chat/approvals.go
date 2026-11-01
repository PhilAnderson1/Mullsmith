package chat

import "sync"

// Approvals implements "users may Remember the choice per
// conversation" (§6.1 step 5, §9.9 layer 6). Deliberately in-memory only:
// a server restart re-opens every consent prompt (a restart is a fresh
// consent window), and entries drop with the conversation. Remembering is
// offered for code_run only — the other confirm-grade tools stay gated.
type Approvals struct {
	mu     sync.Mutex
	byConv map[string]map[string]bool // convID → tool → approved
}

func NewApprovals() *Approvals {
	return &Approvals{byConv: map[string]map[string]bool{}}
}

// bounded: a long-lived server must not grow one entry per conversation
// forever; past the cap new conversations simply never Remember.
const approvalMemoryCap = 4096

func (m *Approvals) Remember(convID, tool string) {
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

func (m *Approvals) Remembered(convID, tool string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byConv[convID][tool]
}

func (m *Approvals) Forget(convID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.byConv, convID)
}
