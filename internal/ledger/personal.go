package ledger

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"time"
)

const personalSchema = `CREATE TABLE IF NOT EXISTS mail_messages (
 id TEXT PRIMARY KEY, source TEXT NOT NULL, external_id TEXT NOT NULL,
 sender TEXT NOT NULL, subject TEXT NOT NULL, body TEXT NOT NULL,
 received_at DATETIME NOT NULL, unread INTEGER NOT NULL,
 UNIQUE(source, external_id)
);
CREATE INDEX IF NOT EXISTS idx_mail_received ON mail_messages(received_at);`

// MailMessage is a bounded plain-text copy. Remote HTML and attachments are never rendered.
type MailMessage struct {
	ID, Source, ExternalID, Sender, Subject, Body string
	ReceivedAt                                    time.Time
	Unread                                        bool
}

func PersonalID(source, external string) string {
	return fmt.Sprintf("%s-%x", source, sha256.Sum256([]byte(external)))
}

// ReplaceMailSnapshot commits only a fully fetched inbox window. Removed and
// archived messages leave the local inbox, including on UIDVALIDITY changes.
func (s *Store) ReplaceMailSnapshot(source string, messages []MailMessage) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`DELETE FROM mail_messages WHERE source=?`, source); err != nil {
		return err
	}
	for _, m := range messages {
		if _, err = tx.Exec(`INSERT INTO mail_messages(id,source,external_id,sender,subject,body,received_at,unread) VALUES(?,?,?,?,?,?,?,?)`, PersonalID(source, m.ExternalID), source, m.ExternalID, m.Sender, m.Subject, m.Body, m.ReceivedAt.UTC(), m.Unread); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) RecentMail(since time.Time, limit int) ([]MailMessage, error) {
	if limit < 1 || limit > 400 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT id,source,external_id,sender,subject,body,received_at,unread FROM mail_messages WHERE received_at>=? ORDER BY received_at DESC,id LIMIT ?`, since.UTC(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MailMessage
	for rows.Next() {
		var m MailMessage
		if err = rows.Scan(&m.ID, &m.Source, &m.ExternalID, &m.Sender, &m.Subject, &m.Body, &m.ReceivedAt, &m.Unread); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ReplaceCalendarSnapshot atomically replaces a complete provider window.
// Existing local job associations survive updates. No other source is touched.
func (s *Store) ReplaceCalendarSnapshot(source string, from, to time.Time, appts []Appointment) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`CREATE TEMP TABLE IF NOT EXISTS personal_keep (id TEXT PRIMARY KEY)`); err != nil {
		return err
	}
	if _, err = tx.Exec(`DELETE FROM personal_keep`); err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, a := range appts {
		if !a.ExternalID.Valid || a.StartsAt.IsZero() {
			return fmt.Errorf("invalid calendar snapshot")
		}
		if _, err = tx.Exec(`INSERT INTO personal_keep(id) VALUES(?)`, a.ExternalID.String); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO appointments(id,title,starts_at,ends_at,location,source,external_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(source,external_id) WHERE external_id IS NOT NULL DO UPDATE SET title=excluded.title,starts_at=excluded.starts_at,ends_at=excluded.ends_at,location=excluded.location,updated_at=excluded.updated_at`, PersonalID(source, a.ExternalID.String), a.Title, a.StartsAt.UTC(), a.EndsAt, a.Location, source, a.ExternalID, now, now); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`DELETE FROM appointments WHERE source=? AND starts_at>=? AND starts_at<? AND external_id NOT IN (SELECT id FROM personal_keep)`, source, from.UTC(), to.UTC()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DeletePersonalConnection(source string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{`DELETE FROM mail_messages WHERE source=?`, `DELETE FROM appointments WHERE source=?`, `DELETE FROM connections WHERE provider=?`} {
		if _, err = tx.Exec(q, source); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RecordPersonalSync leaves the last successful timestamp intact on failure.
func (s *Store) RecordPersonalSync(source string, syncErr error) error {
	if syncErr != nil {
		_, err := s.db.Exec(`UPDATE connections SET last_error=? WHERE provider=?`, syncErr.Error(), source)
		return err
	}
	_, err := s.db.Exec(`UPDATE connections SET last_error=NULL,last_sync_at=? WHERE provider=?`, time.Now().UTC(), source)
	return err
}

func NullText(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }
