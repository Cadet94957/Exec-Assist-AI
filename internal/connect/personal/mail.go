package personal

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/Cadet94957/Exec-Assist-AI/internal/ledger"
	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	_ "github.com/emersion/go-message/charset"
	"github.com/emersion/go-message/mail"
	"golang.org/x/net/html"
)

// EXAMINE + BODY.PEEK preserve read flags. Only INBOX is read; no SMTP client exists.
func fetchMail(ctx context.Context, host string, c Credential, now time.Time) ([]ledger.MailMessage, error) {
	hostname, _, err := net.SplitHostPort(host)
	if err != nil {
		return nil, err
	}
	dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: 15 * time.Second}, Config: &tls.Config{ServerName: hostname, MinVersion: tls.VersionTLS12}}
	conn, err := dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err = conn.SetDeadline(time.Now().Add(60 * time.Second)); err != nil {
		return nil, err
	}
	cl, err := client.New(conn)
	if err != nil {
		return nil, err
	}
	cl.Timeout = 20 * time.Second
	return readMailbox(cl, c, now)
}

func readMailbox(cl *client.Client, c Credential, now time.Time) ([]ledger.MailMessage, error) {
	if err := cl.Login(c.Account, c.Password); err != nil {
		return nil, err
	}
	box, err := cl.Select("INBOX", true)
	if err != nil {
		return nil, err
	}
	criteria := imap.NewSearchCriteria()
	criteria.Since = now.AddDate(0, 0, -30)
	ids, err := cl.UidSearch(criteria)
	if err != nil {
		return nil, err
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if len(ids) > MailLimit {
		ids = ids[len(ids)-MailLimit:]
	}
	if len(ids) == 0 {
		return nil, nil
	}
	set := new(imap.SeqSet)
	set.AddNum(ids...)
	section := &imap.BodySectionName{Peek: true, Partial: []int{0, 64 << 10}}
	messages := make(chan *imap.Message, MailLimit)
	done := make(chan error, 1)
	go func() {
		done <- cl.UidFetch(set, []imap.FetchItem{imap.FetchUid, imap.FetchEnvelope, imap.FetchInternalDate, imap.FetchFlags, section.FetchItem()}, messages)
	}()
	var out []ledger.MailMessage
	var incomplete bool
	for msg := range messages {
		if msg == nil || msg.Envelope == nil || msg.Uid == 0 {
			incomplete = true
			continue
		}
		e := msg.Envelope
		m := ledger.MailMessage{ExternalID: fmt.Sprintf("INBOX:%d:%d", box.UidValidity, msg.Uid), Subject: Clean(e.Subject, 300), ReceivedAt: msg.InternalDate, Unread: true}
		if m.ReceivedAt.IsZero() {
			m.ReceivedAt = e.Date
		}
		if m.ReceivedAt.IsZero() {
			m.ReceivedAt = now
		}
		for _, flag := range msg.Flags {
			if strings.EqualFold(flag, imap.SeenFlag) {
				m.Unread = false
			}
		}
		var senders []string
		for _, addr := range e.From {
			senders = append(senders, addr.PersonalName+" <"+addr.Address()+">")
		}
		m.Sender = Clean(strings.Join(senders, ", "), 400)
		if body := msg.GetBody(section); body != nil {
			m.Body = preview(io.LimitReader(body, 64<<10))
		}
		if m.Subject == "" {
			m.Subject = "(no subject)"
		}
		out = append(out, m)
	}
	if err = <-done; err != nil {
		return nil, err
	}
	if incomplete {
		return nil, fmt.Errorf("incomplete mail response")
	}
	// A message can vanish between SEARCH and FETCH; the remaining complete
	// result is a valid current inbox snapshot. UIDVALIDITY is part of identity.
	return out, nil
}

func preview(r io.Reader) string {
	reader, _ := mail.CreateReader(r)
	if reader == nil {
		return "(Message preview unavailable)"
	}
	defer reader.Close()
	var fallback string
	for i := 0; i < 40; i++ {
		part, err := reader.NextPart()
		if err != nil {
			break
		}
		h, ok := part.Header.(*mail.InlineHeader)
		if !ok {
			continue
		}
		kind, _, _ := h.ContentType()
		body, _ := io.ReadAll(io.LimitReader(part.Body, 16<<10))
		if kind == "text/plain" {
			return Clean(string(body), 8000)
		}
		if kind == "text/html" {
			fallback = htmlText(string(body))
		}
	}
	if fallback != "" {
		return Clean(fallback, 8000)
	}
	return "(No text preview in the first 64 KiB; open your mail app for the full message)"
}
func htmlText(raw string) string {
	z := html.NewTokenizer(strings.NewReader(raw))
	var b strings.Builder
	skip := 0
	for {
		switch z.Next() {
		case html.ErrorToken:
			return b.String()
		case html.StartTagToken:
			t := z.Token()
			if t.Data == "script" || t.Data == "style" {
				skip++
			}
			b.WriteByte(' ')
		case html.EndTagToken:
			t := z.Token()
			if (t.Data == "script" || t.Data == "style") && skip > 0 {
				skip--
			}
			b.WriteByte(' ')
		case html.TextToken:
			if skip == 0 {
				b.Write(z.Text())
				b.WriteByte(' ')
			}
		}
	}
}
