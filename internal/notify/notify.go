// Package notify sends failure alerts and weekly reports by e-mail, either via
// SMTP or via Microsoft Graph (sendMail as the backed-up account).
package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ulbrich/ms365backupd/internal/config"
	"github.com/ulbrich/ms365backupd/internal/graph"
)

// Message is a notification with a plain-text body and an optional HTML version of it.
type Message struct {
	Subject string
	Text    string
	HTML    string // empty: text only
}

// Sender delivers a message to the configured recipients.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// GraphFactory lazily provides a Graph client (needed only for via=graph).
type GraphFactory func(ctx context.Context) (*graph.Client, error)

// New builds the sender for cfg, or nil if notifications are disabled.
func New(cfg *config.Config, gf GraphFactory) Sender {
	if !cfg.EmailEnabled() {
		return nil
	}
	if cfg.EmailVia() == "graph" {
		return &graphSender{cfg: cfg, gf: gf}
	}
	return &smtpSender{cfg: cfg}
}

type smtpSender struct{ cfg *config.Config }

func (s *smtpSender) Send(ctx context.Context, m Message) error {
	e := s.cfg.Email
	port := e.SMTPPort
	mode := strings.ToLower(e.TLS)
	if mode == "" {
		mode = "starttls"
	}
	if port == 0 {
		switch mode {
		case "tls":
			port = 465
		case "none":
			port = 25
		default:
			port = 587
		}
	}
	addr := net.JoinHostPort(e.SMTPHost, strconv.Itoa(port))
	dl := &net.Dialer{Timeout: 30 * time.Second}
	var conn net.Conn
	var err error
	if mode == "tls" {
		conn, err = tls.DialWithDialer(dl, "tcp", addr, &tls.Config{ServerName: e.SMTPHost})
	} else {
		conn, err = dl.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("smtp connect %s: %w", addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(2 * time.Minute))
	c, err := smtp.NewClient(conn, e.SMTPHost)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	host, _ := os.Hostname()
	if host != "" {
		_ = c.Hello(host)
	}
	if mode == "starttls" {
		if err := c.StartTLS(&tls.Config{ServerName: e.SMTPHost}); err != nil {
			return fmt.Errorf("starttls: %w", err)
		}
	}
	if e.Username != "" {
		pw, err := s.cfg.SMTPPassword()
		if err != nil {
			return fmt.Errorf("smtp password: %w", err)
		}
		if mode == "none" {
			return errors.New("refusing to send SMTP credentials without TLS")
		}
		if err := c.Auth(smtp.PlainAuth("", e.Username, pw, e.SMTPHost)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := c.Mail(e.From); err != nil {
		return err
	}
	for _, to := range e.To {
		if err := c.Rcpt(to); err != nil {
			return fmt.Errorf("rcpt %s: %w", to, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(buildMessage(e.From, e.To, m)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// buildMessage writes the RFC 5322 message: text/plain, or multipart/alternative with
// the text part first and the HTML part last (the preferred one).
func buildMessage(from string, to []string, m Message) []byte {
	var b bytes.Buffer
	var id [12]byte
	_, _ = rand.Read(id[:])
	subject := m.Subject
	domain := "localhost"
	if i := strings.LastIndex(from, "@"); i >= 0 {
		domain = strings.Trim(from[i+1:], "> ")
	}
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", mime.QEncoding.Encode("utf-8", subject))
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&b, "Message-ID: <%s@%s>\r\n", hex.EncodeToString(id[:]), domain)
	b.WriteString("MIME-Version: 1.0\r\nAuto-Submitted: auto-generated\r\nX-Mailer: ms365backupd\r\n")
	part := func(ctype, body string) {
		fmt.Fprintf(&b, "Content-Type: %s; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", ctype)
		qp := quotedprintable.NewWriter(&b)
		_, _ = qp.Write([]byte(strings.ReplaceAll(body, "\n", "\r\n")))
		_ = qp.Close()
	}
	if m.HTML == "" {
		part("text/plain", m.Text)
		return b.Bytes()
	}
	boundary := "ms365backupd-" + hex.EncodeToString(id[:])
	fmt.Fprintf(&b, "Content-Type: multipart/alternative; boundary=\"%s\"\r\n\r\n", boundary)
	fmt.Fprintf(&b, "--%s\r\n", boundary)
	part("text/plain", m.Text)
	fmt.Fprintf(&b, "\r\n--%s\r\n", boundary)
	part("text/html", m.HTML)
	fmt.Fprintf(&b, "\r\n--%s--\r\n", boundary)
	return b.Bytes()
}

type graphSender struct {
	cfg *config.Config
	gf  GraphFactory
}

// Send posts the message via Graph sendMail. Graph takes one body: the HTML version if
// there is one (Exchange adds a text part for recipients that need it), else the text.
func (s *graphSender) Send(ctx context.Context, m Message) error {
	g, err := s.gf(ctx)
	if err != nil {
		return err
	}
	type addr struct {
		EmailAddress struct {
			Address string `json:"address"`
		} `json:"emailAddress"`
	}
	var to []addr
	for _, t := range s.cfg.Email.To {
		var a addr
		a.EmailAddress.Address = t
		to = append(to, a)
	}
	body := map[string]string{"contentType": "Text", "content": m.Text}
	if m.HTML != "" {
		body = map[string]string{"contentType": "HTML", "content": m.HTML}
	}
	msg := map[string]any{
		"message": map[string]any{
			"subject":      m.Subject,
			"body":         body,
			"toRecipients": to,
		},
		"saveToSentItems": false,
	}
	return g.PostJSON(ctx, "me/sendMail", msg, nil)
}
