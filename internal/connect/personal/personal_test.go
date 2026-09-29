package personal

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Cadet94957/Exec-Assist-AI/internal/ledger"
	"github.com/Cadet94957/Exec-Assist-AI/internal/secrets"
	"github.com/emersion/go-imap/client"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func multi(href, prop string) string {
	return `<d:multistatus xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:response><d:href>` + href + `</d:href><d:propstat><d:prop>` + prop + `</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response></d:multistatus>`
}
func TestCalendarDiscoveryAndExpandedEvents(t *testing.T) {
	now := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	calls := 0
	cl := calendarClient{credential: Credential{"user@icloud.com", "test-app-password"}}
	cl.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		u, p, ok := r.BasicAuth()
		if !ok || u != "user@icloud.com" || p != "test-app-password" {
			t.Fatal("missing auth")
		}
		var body string
		switch r.URL.Path {
		case "/":
			body = multi("/", `<d:current-user-principal><d:href>/123/principal/</d:href></d:current-user-principal>`)
		case "/123/principal/":
			body = multi(r.URL.Path, `<c:calendar-home-set><d:href>https://p01-caldav.icloud.com/123/calendars/</d:href></c:calendar-home-set>`)
		case "/123/calendars/":
			body = multi("/123/calendars/work/", `<d:resourcetype><d:collection/><c:calendar/></d:resourcetype>`)
		case "/123/calendars/work/":
			data, _ := io.ReadAll(r.Body)
			if r.Method != "REPORT" || !strings.Contains(string(data), "c:expand") {
				t.Fatal("must request expanded read-only report")
			}
			body = multi("one.ics", "<c:calendar-data><![CDATA["+expandedICS+"]]></c:calendar-data>")
		default:
			t.Fatalf("unexpected URL %s", r.URL)
		}
		if r.Method != "REPORT" && r.Method != "PROPFIND" {
			t.Fatal("remote write")
		}
		return &http.Response{StatusCode: 207, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	loc, _ := time.LoadLocation("America/Los_Angeles")
	snap, err := cl.snapshot(context.Background(), loc, now)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 4 || len(snap.Appointments) != 3 {
		t.Fatalf("calls=%d events=%+v", calls, snap.Appointments)
	}
	if snap.Appointments[0].ExternalID == snap.Appointments[1].ExternalID {
		t.Fatal("recurrences collided")
	}
	if snap.Appointments[2].StartsAt.Hour() != 7 {
		t.Fatal("all-day date shifted out of tenant timezone")
	}
	if snap.Appointments[0].Title != "Weekly, check-in" {
		t.Fatalf("iCal unescaping: %q", snap.Appointments[0].Title)
	}
}

const expandedICS = `BEGIN:VCALENDAR
VERSION:2.0
BEGIN:VEVENT
UID:weekly
RECURRENCE-ID:20260929T160000Z
DTSTART:20260929T160000Z
DTEND:20260929T170000Z
SUMMARY:Weekly\, check-in
END:VEVENT
BEGIN:VEVENT
UID:weekly
RECURRENCE-ID:20261006T160000Z
DTSTART:20261006T180000Z
DTEND:20261006T190000Z
SUMMARY:Moved weekly
END:VEVENT
BEGIN:VEVENT
UID:holiday
DTSTART;VALUE=DATE:20260930
DTEND;VALUE=DATE:20261001
SUMMARY:Holiday
END:VEVENT
END:VCALENDAR
`

func TestCalendarRefusesIncompleteRecurrenceAndForeignHosts(t *testing.T) {
	now := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	raw := strings.Replace(expandedICS, "SUMMARY:Moved weekly", "RRULE:FREQ=WEEKLY\nSUMMARY:Moved weekly", 1)
	if _, err := parseEvents(raw, "calendar", time.UTC, now.AddDate(0, 0, -1), now.AddDate(0, 0, 60)); err == nil {
		t.Fatal("accepted unexpanded recurrence")
	}
	for _, u := range []string{"http://caldav.icloud.com/", "https://evil.test/", "https://caldav.icloud.com.evil.test/", "https://p01-caldav.icloud.com:8443/", "https://u:p@caldav.icloud.com/"} {
		if appleURL(u) == nil {
			t.Fatalf("allowed credential destination %s", u)
		}
	}
}
func testStore(t *testing.T) *ledger.Store {
	t.Helper()
	s, err := ledger.Open(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func TestConnectionEncryptionSyncFailureAndDisconnect(t *testing.T) {
	store := testStore(t)
	box, _ := secrets.NewBox(strings.Repeat("ab", 32))
	service := New(box)
	fail := false
	service.fetch = func(context.Context, string, Credential, *time.Location, time.Time) (Snapshot, error) {
		if fail {
			return Snapshot{}, errors.New("network unavailable")
		}
		return Snapshot{Messages: []ledger.MailMessage{{ExternalID: "1:1", Subject: "Hesai next steps", Body: "Please reply by Friday", ReceivedAt: time.Now()}}}, nil
	}
	c := Credential{"user@gmail.com", "abcdefghijklmnop"}
	if err := service.Connect(context.Background(), store, Gmail, c, time.UTC); err != nil {
		t.Fatal(err)
	}
	conn, _ := store.GetConnection(Gmail)
	if strings.Contains(string(conn.Token), c.Password) {
		t.Fatal("plaintext password persisted")
	}
	fail = true
	if err := service.Sync(context.Background(), store, Gmail, time.UTC); err == nil {
		t.Fatal("failure swallowed")
	}
	later, _ := store.GetConnection(Gmail)
	if later.LastSync != conn.LastSync || !later.LastError.Valid {
		t.Fatal("failure obscured last success")
	}
	messages, _ := store.RecentMail(time.Time{}, 10)
	if len(messages) != 1 {
		t.Fatal("failed sync deleted mail")
	}
	found, _ := store.SearchBusiness("Hesai")
	if len(found) != 1 || found[0].Kind != "email" {
		t.Fatal("mail missing from search")
	}
	if err := service.Disconnect(store, Gmail); err != nil {
		t.Fatal(err)
	}
	if err := service.Sync(context.Background(), store, Gmail, time.UTC); !errors.Is(err, ledger.ErrNoConnection) {
		t.Fatal("disconnected account still syncs")
	}
	messages, _ = store.RecentMail(time.Time{}, 10)
	if len(messages) != 0 {
		t.Fatal("disconnect retained inbox")
	}
}
func TestIMAPReadOnlyTranscriptAndMIMEPreview(t *testing.T) {
	server, conn := net.Pipe()
	defer conn.Close()
	transcript := make(chan []string, 1)
	raw := "From: Sender <sender@example.com>\r\nSubject: Hello\r\nMIME-Version: 1.0\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>Meeting &amp; lunch</p><script>steal()</script>"
	go func() {
		defer server.Close()
		reader := bufio.NewReader(server)
		fmt.Fprint(server, "* OK test IMAP ready\r\n")
		var commands []string
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				break
			}
			commands = append(commands, line)
			fields := strings.Fields(line)
			tag := fields[0]
			command := strings.ToUpper(strings.Join(fields[1:], " "))
			switch {
			case strings.HasPrefix(command, "LOGIN "):
				fmt.Fprintf(server, "%s OK logged in\r\n", tag)
			case strings.HasPrefix(command, "EXAMINE "):
				fmt.Fprintf(server, "* FLAGS (\\Seen)\r\n* 1 EXISTS\r\n* OK [UIDVALIDITY 9] valid\r\n%s OK [READ-ONLY] selected\r\n", tag)
			case strings.HasPrefix(command, "UID SEARCH "):
				fmt.Fprintf(server, "* SEARCH 42\r\n%s OK search\r\n", tag)
			case strings.HasPrefix(command, "UID FETCH "):
				fmt.Fprintf(server, "* 1 FETCH (UID 42 FLAGS () INTERNALDATE \"29-Sep-2026 01:00:00 +0000\" ENVELOPE (NIL \"Hello\" ((\"Sender\" NIL \"sender\" \"example.com\")) NIL NIL NIL NIL NIL NIL \"<id@example.com>\") BODY[]<0> {%d}\r\n%s)\r\n%s OK fetched\r\n", len(raw), raw, tag)
				transcript <- commands
				return
			default:
				fmt.Fprintf(server, "%s BAD unsupported\r\n", tag)
			}
		}
	}()
	cl, err := client.New(conn)
	if err != nil {
		t.Fatal(err)
	}
	cl.Timeout = 2 * time.Second
	msgs, err := readMailbox(cl, Credential{"user@gmail.com", "test-app-password"}, time.Date(2026, 9, 29, 2, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].ExternalID != "INBOX:9:42" || !msgs[0].Unread || msgs[0].Body != "Meeting & lunch" {
		t.Fatalf("bad preview %+v", msgs)
	}
	commands := strings.Join(<-transcript, "")
	if !strings.Contains(commands, "EXAMINE") || !strings.Contains(commands, "BODY.PEEK[]<0.65536>") {
		t.Fatalf("read-only bounded fetch absent: %s", commands)
	}
	for _, write := range []string{" SELECT ", " STORE ", " EXPUNGE ", " APPEND "} {
		if strings.Contains(commands, write) {
			t.Fatal("remote mutation")
		}
	}
}
