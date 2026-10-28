package mail

import (
	"log/slog"
	"time"

	"gabkeep/internal/config"
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
}

const (
	queueSize = 64
)

var defaultBackoff = []time.Duration{10 * time.Second, 60 * time.Second}

// NewOutbox starts the worker goroutine for a (test or production)
// Deliverer.
func NewOutbox(d Deliverer, logger *slog.Logger) *Outbox {
	o := &Outbox{
		del:     d,
		logger:  logger,
		ch:      make(chan Job, queueSize),
		backoff: defaultBackoff,
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
// path must never block or 500 on a mail problem).
func (o *Outbox) Send(to, subject, body string) {
	select {
	case o.ch <- Job{To: to, Subject: subject, Body: body}:
	default:
		if o.logger != nil {
			o.logger.Warn("mail queue full — message dropped (user can retry)", "subject", subject)
		}
	}
}

func (o *Outbox) work() {
	for j := range o.ch {
		if o.cleartextWarn && o.logger != nil {
			o.logger.Warn("sending mail unauthenticated (email.smtp.authenticated_smtp=false);"+
				" STARTTLS is used when offered", "host_subject", j.Subject)
		}
		for attempt := 0; ; attempt++ {
			err := o.del.Deliver(j.To, j.Subject, j.Body)
			if err == nil {
				break
			}
			if attempt >= len(o.backoff) {
				if o.logger != nil {
					// Recipients are not logged (log files outlive accounts).
					o.logger.Error("mail delivery failed after retries — dropping",
						"subject", j.Subject, "attempts", attempt+1, "error", err)
				}
				break
			}
			time.Sleep(o.backoff[attempt])
		}
	}
}
