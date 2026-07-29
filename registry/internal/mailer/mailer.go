// Package mailer is the registry's outbound email path.
//
// KISS: one interface, two implementations (NoopMailer for dev/test,
// smtpMailer for production). No DI framework, no pluggable backend
// registry, no SendGrid/SES adapter — stdlib net/smtp is enough for
// a self-hosted registry that talks to a local smarthost.
//
// The only caller is the auth handler's email-validation flow
// (Register + ResendVerification). When email_validation.enable_validation
// is false the mailer is never invoked; when it's true and SMTP.Host
// is empty, NoopMailer logs the verification URL to stdout so a
// local registry boots without a mail server (mirrors the
// presign_base_url "disable" convenience in main.go).
package mailer

import (
	"fmt"
	"log"
	"net/smtp"
	"strings"

	"github.com/openktree/knowledge-registry/internal/config"
)

// Mailer sends a plain-text email. The body is built by the caller
// (the auth handler) — no template engine, no HTML, no headers
// beyond From/To/Subject. The verification URL is the only thing
// that matters.
type Mailer interface {
	Send(to, subject, body string) error
}

// Message is a captured email. NoopMailer appends to SentMessages so
// tests can assert on the verification URL without a real SMTP
// server. Production code never reads SentMessages.
type Message struct {
	To      string
	Subject string
	Body    string
}

// NewFromConfig picks the implementation: empty SMTP.Host → NoopMailer
// (dev/test), otherwise smtpMailer. Never returns an error — the
// NoopMailer fallback means a misconfigured SMTP block degrades to
// logging instead of crashing the boot. The FromAddress from the
// EmailValidationConfig is used as the envelope + message From; the
// config layer defaults it to "no-reply@localhost".
func NewFromConfig(cfg config.EmailValidationConfig) Mailer {
	if cfg.SMTP.Host == "" {
		return &NoopMailer{}
	}
	return &smtpMailer{from: cfg.FromAddress, cfg: cfg.SMTP}
}

// NoopMailer logs each Send to stdout and records it in
// SentMessages. The log line is the dev path: an operator running
// the registry locally with email_validation on but no SMTP server
// sees the verification URL in the logs and can click it directly.
type NoopMailer struct {
	// SentMessages is a test affordance; production code never
	// reads it. Tests assert on SentMessages[0].Body to extract
	// the verification URL.
	SentMessages []Message
}

func (m *NoopMailer) Send(to, subject, body string) error {
	m.SentMessages = append(m.SentMessages, Message{To: to, Subject: subject, Body: body})
	log.Printf("mailer(noop): to=%s subject=%q\n--- body ---\n%s\n--- end ---", to, subject, body)
	return nil
}

type smtpMailer struct {
	from string
	cfg  config.SMTPConfig
}

func (m *smtpMailer) Send(to, subject, body string) error {
	addr := fmt.Sprintf("%s:%d", m.cfg.Host, m.cfg.Port)
	// PLAIN auth when credentials are configured; anonymous
	// delivery otherwise (a local smarthost on the same host
	// often allows unauthenticated relay). net/smtp.SendMail
	// handles STARTTLS when the server advertises it. Implicit-TLS
	// (smtps on 465) isn't supported by stdlib net/smtp directly;
	// an operator who needs smtps can front the registry with a
	// local relay that speaks STARTTLS upstream. The TLS toggle
	// is reserved for a future implicit-TLS path; today it's a
	// no-op flag so the config round-trips without breaking.
	var auth smtp.Auth
	if m.cfg.Username != "" || m.cfg.Password != "" {
		auth = smtp.PlainAuth("", m.cfg.Username, m.cfg.Password, m.cfg.Host)
	}
	msg := strings.Join([]string{
		"From: " + m.from,
		"To: " + to,
		"Subject: " + subject,
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"",
		body,
	}, "\r\n")
	return smtp.SendMail(addr, auth, m.from, []string{to}, []byte(msg))
}
