// Package personal supplies read-only iCloud Calendar and IMAP inbox adapters.
// App passwords are encrypted at rest; they are not scope-limited by Apple or
// Google. The adapters themselves never write to either remote service.
package personal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"sync"
	"time"

	"github.com/Cadet94957/Exec-Assist-AI/internal/ledger"
	"github.com/Cadet94957/Exec-Assist-AI/internal/secrets"
)

const (
	AppleCalendar = "icloud-calendar"
	AppleMail     = "icloud-mail"
	Gmail         = "gmail"
	MailLimit     = 200
)

var Providers = []string{AppleCalendar, AppleMail, Gmail}

func Valid(provider string) bool {
	for _, p := range Providers {
		if p == provider {
			return true
		}
	}
	return false
}
func Name(p string) string {
	switch p {
	case AppleCalendar:
		return "Apple Calendar (iCloud)"
	case AppleMail:
		return "Apple Mail (iCloud)"
	case Gmail:
		return "Gmail"
	}
	return p
}

type Credential struct {
	Account  string
	Password string
}
type Snapshot struct {
	Appointments []ledger.Appointment
	Messages     []ledger.MailMessage
	From, To     time.Time
}
type Fetcher func(context.Context, string, Credential, *time.Location, time.Time) (Snapshot, error)
type Service struct {
	box   *secrets.Box
	fetch Fetcher
	locks sync.Map
}

func New(box *secrets.Box) *Service { return &Service{box: box, fetch: fetch} }
func (s *Service) lock(store *ledger.Store, provider string) func() {
	key := struct {
		Store    *ledger.Store
		Provider string
	}{store, provider}
	m, _ := s.locks.LoadOrStore(key, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}
func (s *Service) Ready() bool { return s.box != nil }
func (s *Service) Connect(ctx context.Context, store *ledger.Store, provider string, c Credential, loc *time.Location) error {
	if !Valid(provider) {
		return errors.New("Unknown connection")
	}
	if s.box == nil {
		return secrets.ErrNoKey
	}
	c.Account = strings.TrimSpace(c.Account)
	c.Password = strings.ReplaceAll(strings.TrimSpace(c.Password), " ", "")
	addr, err := mail.ParseAddress(c.Account)
	if err != nil || addr.Address != c.Account || len(c.Account) > 254 || len(c.Password) < 12 || len(c.Password) > 128 {
		return errors.New("Enter your full account email and an app-specific password")
	}
	unlock := s.lock(store, provider)
	defer unlock()
	old, err := store.GetConnection(provider)
	if err == nil && !strings.EqualFold(old.Account, c.Account) {
		return errors.New("Disconnect the current account before connecting a different one")
	}
	if err != nil && !errors.Is(err, ledger.ErrNoConnection) {
		return err
	}
	snap, err := s.fetch(ctx, provider, c, loc, time.Now())
	if err != nil {
		return err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	sealed, err := s.box.Seal(raw)
	if err != nil {
		return err
	}
	if err = store.SaveConnection(provider, c.Account, sealed); err != nil {
		return err
	}
	if err = s.apply(store, provider, snap); err != nil {
		store.RecordPersonalSync(provider, err)
		return err
	}
	return store.RecordPersonalSync(provider, nil)
}
func (s *Service) Sync(ctx context.Context, store *ledger.Store, provider string, loc *time.Location) error {
	if !Valid(provider) {
		return errors.New("Unknown connection")
	}
	if s.box == nil {
		return secrets.ErrNoKey
	}
	unlock := s.lock(store, provider)
	defer unlock()
	conn, err := store.GetConnection(provider)
	if err != nil {
		return err
	}
	raw, err := s.box.Open(conn.Token)
	if err != nil {
		store.RecordPersonalSync(provider, err)
		return err
	}
	var c Credential
	if err = json.Unmarshal(raw, &c); err != nil {
		return err
	}
	snap, err := s.fetch(ctx, provider, c, loc, time.Now())
	if err == nil {
		err = s.apply(store, provider, snap)
	}
	if markErr := store.RecordPersonalSync(provider, err); err == nil {
		err = markErr
	}
	return err
}
func (s *Service) apply(store *ledger.Store, provider string, snap Snapshot) error {
	if provider == AppleCalendar {
		return store.ReplaceCalendarSnapshot(provider, snap.From, snap.To, snap.Appointments)
	}
	return store.ReplaceMailSnapshot(provider, snap.Messages)
}
func (s *Service) Disconnect(store *ledger.Store, provider string) error {
	if !Valid(provider) {
		return errors.New("Unknown connection")
	}
	unlock := s.lock(store, provider)
	defer unlock()
	return store.DeletePersonalConnection(provider)
}
func fetch(ctx context.Context, p string, c Credential, loc *time.Location, now time.Time) (Snapshot, error) {
	if p == AppleCalendar {
		return fetchCalendar(ctx, c, loc, now)
	}
	host := "imap.mail.me.com:993"
	if p == Gmail {
		host = "imap.gmail.com:993"
	}
	messages, err := fetchMail(ctx, host, c, now)
	if err != nil {
		return Snapshot{}, fmt.Errorf("%s could not sync. Check your app password, IMAP access, and network connection", Name(p))
	}
	return Snapshot{Messages: messages}, nil
}

// Clean strips control characters and bounds untrusted text before storage or prompts.
func Clean(s string, n int) string {
	s = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return ' '
		}
		return r
	}, s)), " ")
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
