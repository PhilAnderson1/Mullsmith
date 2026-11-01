package mail

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"mullsmith/internal/config"
)

// Job is one queued plain-text message.
type Job struct {
	To, Subject, Body string
}

// Outbox decouples mail from HTTP: handlers enqueue and answer immediately,
// one worker dialing with retries. Mail that still fails after all attempts
// is an operator-log problem — a durable queue would be overkill for a
// self-hosted tool (plan §4); the user-facing response is already the
// generic "a mail is on its way" and the link stays minted, so `resend`
// works once SMTP recovers.
type Outbox struct {
	del     Deliverer
	logger  *slog.Logger
	ch      chan Job
	backoff []time.Duration
	// cleartextWarn marks the unauthenticated (LAN-relay) config: legal,
	// but every send logs the advisory.
	cleartextWarn bool
	// Shutdown coordination (R5, bounded per review3 issue 5): Close signals
	// the worker, CANCELS the active delivery through runCtx (a ctx-honest
	// Deliverer stops mid-SMTP), and joins it within the caller's budget —
	// no unlimited wait is added after the HTTP/generation grace periods.
	// Pending work is ABANDONED with a log line, per the best-effort mail
	// policy (the user can resend).
	quit      chan struct{}
	done      chan struct{}
	runCtx    context.Context
	runCancel context.CancelFunc
	closeMu   sync.Mutex
	closeOnce bool
}

const (
	queueSize = 64
)

var defaultBackoff = []time.Duration{10 * time.Second, 60 * time.Second}

// NewOutbox starts the worker goroutine for a (test or production)
// Deliverer.
func NewOutbox(d Deliverer, logger *slog.Logger) *Outbox {
	runCtx, runCancel := context.WithCancel(context.Background())
	o := &Outbox{
		del:       d,
		logger:    logger,
		ch:        make(chan Job, queueSize),
		backoff:   defaultBackoff,
		quit:      make(chan struct{}),
		done:      make(chan struct{}),
		runCtx:    runCtx,
		runCancel: runCancel,
	}
	go o.work()
	return o
}

// NewForConfig wires the production SMTP deliverer for an enabled email
// section. Callers gate on cfg.Enabled first.
func NewForConfig(cfg config.Email, logger *slog.Logger) *Outbox {
	o := NewOutbox(NewSMTP(cfg.SMTP), logger)
	o.cleartextWarn = !cfg.SMTP.AuthenticatedSMTP
	return o
}

// Send queues a message; a full queue drops with a warning (the request
// path must never block or 500 on a mail problem). A nil *Outbox (email
// features disabled) accepts and discards: handlers gate on config, and
// the mail layer refuses to be a panic path anyway. After Close the queue
// is shut — drops are logged like any other overload.
func (o *Outbox) Send(to, subject, body string) {
	if o == nil {
		return
	}
	o.closeMu.Lock()
	defer o.closeMu.Unlock()
	if o.closeOnce {
		if o.logger != nil {
			o.logger.Warn("mail queued after shutdown — dropped", "subject", subject)
		}
		return
	}
	select {
	case o.ch <- Job{To: to, Subject: subject, Body: body}:
	default:
		if o.logger != nil {
			o.logger.Warn("mail queue full — message dropped (user can retry)", "subject", subject)
		}
	}
}

// Close stops the worker and joins it within ctx's budget: the retry
// backoff is interrupted, the ACTIVE delivery is cancelled through the
// worker context (ctx-honest Deliverers — the SMTP client here — abort
// mid-conversation), and queued jobs are abandoned with a log line. If the
// deadline passes first, Close returns ctx.Err() and leaves the worker to
// retire on its own: mail is best-effort by policy, and the shutdown
// budget is shared, not extended (review3 issue 5). Close is idempotent
// and nil-safe; Send after Close drops safely whether or not the join
// completed.
func (o *Outbox) Close(ctx context.Context) error {
	if o == nil {
		return nil
	}
	o.closeMu.Lock()
	if !o.closeOnce {
		o.closeOnce = true
		close(o.quit) // stop queue pickup and retry waits
		o.runCancel() // cancel an in-flight delivery
	}
	o.closeMu.Unlock()
	select {
	case <-o.done:
		return nil
	case <-ctx.Done():
		if o.logger != nil {
			o.logger.Warn("shutdown: mail worker still delivering — abandoning it (user can resend)")
		}
		return ctx.Err()
	}
}

func (o *Outbox) work() {
	defer close(o.done)
	for {
		select {
		case <-o.quit:
			if n := len(o.ch); n > 0 && o.logger != nil {
				o.logger.Info("shutdown: abandoning queued mail (user can resend)", "count", n)
			}
			return
		case j := <-o.ch:
			o.deliver(j)
		}
	}
}

// deliver retries one job on the worker context: the backoff sleeps stop
// at shutdown AND the attempt itself is cancellable, so the worker exits
// inside one interrupted I/O cycle rather than one SMTP conversation.
func (o *Outbox) deliver(j Job) {
	if o.cleartextWarn && o.logger != nil {
		o.logger.Warn("sending mail unauthenticated (email.smtp.authenticated_smtp=false);"+
			" STARTTLS is used when offered", "host_subject", j.Subject)
	}
	for attempt := 0; ; attempt++ {
		select {
		case <-o.quit:
			if o.logger != nil {
				o.logger.Info("shutdown: mail delivery abandoned (user can resend)",
					"subject", j.Subject, "attempts", attempt)
			}
			return
		default:
		}
		err := o.del.Deliver(o.runCtx, j.To, j.Subject, j.Body)
		if err == nil {
			break
		}
		if o.runCtx.Err() != nil {
			// The cancellation explains the failure: it is not a delivery
			// error to retry nor a loss to announce.
			if o.logger != nil {
				o.logger.Info("shutdown: mail delivery cancelled mid-flight (user can resend)",
					"subject", j.Subject, "attempts", attempt+1)
			}
			return
		}
		if attempt >= len(o.backoff) {
			if o.logger != nil {
				// Recipients are not logged (log files outlive accounts).
				o.logger.Error("mail delivery failed after retries — dropping",
					"subject", j.Subject, "attempts", attempt+1, "error", err)
			}
			break
		}
		t := time.NewTimer(o.backoff[attempt])
		select {
		case <-t.C:
		case <-o.quit:
			t.Stop()
			if o.logger != nil {
				o.logger.Info("shutdown: mail retry abandoned (user can resend)",
					"subject", j.Subject, "attempts", attempt+1, "error", err)
			}
			return
		}
	}
}
