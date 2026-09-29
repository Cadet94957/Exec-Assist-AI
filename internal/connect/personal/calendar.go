package personal

import (
	"context"
	"database/sql"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Cadet94957/Exec-Assist-AI/internal/ledger"
	"github.com/emersion/go-ical"
)

const davNS = "DAV:"
const calNS = "urn:ietf:params:xml:ns:caldav"

var appleHost = regexp.MustCompile(`^(caldav\.icloud\.com|p[0-9]{1,3}-caldav\.icloud\.com)$`)

type davProperty struct {
	Principal struct {
		Href string `xml:"DAV: href"`
	} `xml:"DAV: current-user-principal"`
	Home struct {
		Href string `xml:"DAV: href"`
	} `xml:"urn:ietf:params:xml:ns:caldav calendar-home-set"`
	Resource struct {
		Calendar *struct{} `xml:"urn:ietf:params:xml:ns:caldav calendar"`
	} `xml:"DAV: resourcetype"`
	Data string `xml:"urn:ietf:params:xml:ns:caldav calendar-data"`
}
type davResponse struct {
	Href   string `xml:"DAV: href"`
	Status string `xml:"DAV: status"`
	Props  []struct {
		Status string      `xml:"DAV: status"`
		Prop   davProperty `xml:"DAV: prop"`
	} `xml:"DAV: propstat"`
}
type multistatus struct {
	XMLName   xml.Name      `xml:"DAV: multistatus"`
	Responses []davResponse `xml:"DAV: response"`
}

type calendarClient struct {
	http       *http.Client
	credential Credential
}

func appleURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !appleHost.MatchString(u.Hostname()) || (u.Port() != "" && u.Port() != "443") || u.User != nil || u.Fragment != "" {
		return errors.New("iCloud returned an unexpected calendar address")
	}
	return nil
}
func resolveApple(base, href string) (string, error) {
	b, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(href)
	if err != nil {
		return "", err
	}
	target := b.ResolveReference(u).String()
	return target, appleURL(target)
}
func fetchCalendar(ctx context.Context, c Credential, loc *time.Location, now time.Time) (Snapshot, error) {
	client := &calendarClient{credential: c}
	client.http = &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 4 {
			return errors.New("Too many iCloud redirects")
		}
		if err := appleURL(req.URL.String()); err != nil {
			return err
		}
		req.SetBasicAuth(c.Account, c.Password)
		return nil
	}}
	snap, err := client.snapshot(ctx, loc, now)
	if err != nil {
		return Snapshot{}, fmt.Errorf("iCloud Calendar could not sync: %w", err)
	}
	return snap, nil
}
func (c *calendarClient) request(ctx context.Context, method, target, depth, body string) ([]davResponse, error) {
	if err := appleURL(target); err != nil {
		return nil, err
	}
	// Neither discovery nor sync ever uses PUT, DELETE, or POST.
	if method != "PROPFIND" && method != "REPORT" {
		return nil, errors.New("Unsupported calendar operation")
	}
	req, err := http.NewRequestWithContext(ctx, method, target, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(c.credential.Account, c.credential.Password)
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	req.Header.Set("Depth", depth)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("Check your network connection and try again")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, errors.New("Check your Apple Account email and app-specific password")
	}
	if resp.StatusCode != 207 {
		return nil, fmt.Errorf("Calendar server returned HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 8<<20 {
		return nil, errors.New("Calendar response is too large; previous data was preserved")
	}
	var result multistatus
	if err = xml.Unmarshal(raw, &result); err != nil {
		return nil, errors.New("Invalid calendar response; previous data was preserved")
	}
	for _, r := range result.Responses {
		if r.Status != "" && !strings.Contains(r.Status, " 200 ") {
			return nil, errors.New("Incomplete calendar response; previous data was preserved")
		}
	}
	return result.Responses, nil
}
func propfind(prop string) string {
	return `<d:propfind xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop>` + prop + `</d:prop></d:propfind>`
}
func (c *calendarClient) snapshot(ctx context.Context, loc *time.Location, now time.Time) (Snapshot, error) {
	from, to := now.AddDate(0, 0, -30), now.AddDate(0, 0, 60)
	root := "https://caldav.icloud.com/"
	rs, err := c.request(ctx, "PROPFIND", root, "0", propfind(`<d:current-user-principal/>`))
	if err != nil {
		return Snapshot{}, err
	}
	principal := ""
	for _, r := range rs {
		for _, p := range r.Props {
			if strings.Contains(p.Status, " 200 ") {
				principal = p.Prop.Principal.Href
			}
		}
	}
	if principal == "" {
		return Snapshot{}, errors.New("Could not discover your iCloud calendar account")
	}
	principal, err = resolveApple(root, principal)
	if err != nil {
		return Snapshot{}, err
	}
	rs, err = c.request(ctx, "PROPFIND", principal, "0", propfind(`<c:calendar-home-set/>`))
	if err != nil {
		return Snapshot{}, err
	}
	home := ""
	for _, r := range rs {
		for _, p := range r.Props {
			if strings.Contains(p.Status, " 200 ") {
				home = p.Prop.Home.Href
			}
		}
	}
	if home == "" {
		return Snapshot{}, errors.New("iCloud Calendar is unavailable for this account")
	}
	home, err = resolveApple(principal, home)
	if err != nil {
		return Snapshot{}, err
	}
	rs, err = c.request(ctx, "PROPFIND", home, "1", propfind(`<d:resourcetype/>`))
	if err != nil {
		return Snapshot{}, err
	}
	var calendars []string
	for _, r := range rs {
		for _, p := range r.Props {
			if !strings.Contains(p.Status, " 200 ") {
				return Snapshot{}, errors.New("Could not read all calendar collections")
			}
			if p.Prop.Resource.Calendar != nil {
				u, err := resolveApple(home, r.Href)
				if err != nil {
					return Snapshot{}, err
				}
				calendars = append(calendars, u)
			}
		}
	}
	if len(calendars) > 50 {
		return Snapshot{}, errors.New("More than 50 calendars; sync limit reached")
	}
	snap := Snapshot{From: from, To: to}
	for _, calendar := range calendars {
		start, end := from.UTC().Format("20060102T150405Z"), to.UTC().Format("20060102T150405Z")
		body := fmt.Sprintf(`<c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><c:calendar-data><c:expand start="%s" end="%s"/></c:calendar-data></d:prop><c:filter><c:comp-filter name="VCALENDAR"><c:comp-filter name="VEVENT"><c:time-range start="%s" end="%s"/></c:comp-filter></c:comp-filter></c:filter></c:calendar-query>`, start, end, start, end)
		resources, err := c.request(ctx, "REPORT", calendar, "1", body)
		if err != nil {
			return Snapshot{}, err
		}
		for _, r := range resources {
			data := ""
			for _, p := range r.Props {
				if strings.Contains(p.Status, " 200 ") {
					data = p.Prop.Data
				}
			}
			if data == "" {
				return Snapshot{}, errors.New("Incomplete event response; previous data was preserved")
			}
			appts, err := parseEvents(data, calendar, loc, from, to)
			if err != nil {
				return Snapshot{}, err
			}
			snap.Appointments = append(snap.Appointments, appts...)
			if len(snap.Appointments) > 5000 {
				return Snapshot{}, errors.New("More than 5000 events; sync limit reached")
			}
		}
	}
	return snap, nil
}
func parseEvents(raw, calendar string, loc *time.Location, from, to time.Time) ([]ledger.Appointment, error) {
	cal, err := ical.NewDecoder(strings.NewReader(raw)).Decode()
	if err != nil {
		return nil, errors.New("Could not parse a calendar event")
	}
	var out []ledger.Appointment
	for _, ev := range cal.Events() {
		status, _ := ev.Props.Text("STATUS")
		if status == "CANCELLED" {
			continue
		}
		// RFC 4791 expansion must return individual instances, not an unexpanded
		// RRULE. Refuse a partial schedule if a server ignores this requirement.
		if ev.Props.Get("RRULE") != nil || ev.Props.Get("RDATE") != nil {
			return nil, errors.New("Server did not expand recurring events; previous data was preserved")
		}
		start, err := ev.DateTimeStart(loc)
		if err != nil || start.IsZero() {
			return nil, errors.New("Unsupported event start/timezone")
		}
		if start.Before(from) || !start.Before(to) {
			continue
		}
		end, err := ev.DateTimeEnd(loc)
		if err != nil {
			return nil, errors.New("Unsupported event end/timezone")
		}
		if startProp := ev.Props.Get("DTSTART"); startProp != nil && startProp.ValueType() == ical.ValueDate && ev.Props.Get("DTEND") == nil && ev.Props.Get("DURATION") == nil {
			end = start.AddDate(0, 0, 1)
		}
		if !end.IsZero() && end.Before(start) {
			return nil, errors.New("Calendar event ends before it starts")
		}
		uid, _ := ev.Props.Text("UID")
		if uid == "" {
			return nil, errors.New("Event is missing its identifier")
		}
		instance := ""
		if p := ev.Props.Get("RECURRENCE-ID"); p != nil {
			t, err := p.DateTime(loc)
			if err != nil {
				return nil, errors.New("Unsupported recurrence timezone")
			}
			instance = t.UTC().Format(time.RFC3339)
		}
		title, _ := ev.Props.Text("SUMMARY")
		location, _ := ev.Props.Text("LOCATION")
		if title == "" {
			title = "(no title)"
		}
		out = append(out, ledger.Appointment{Title: Clean(title, 200), StartsAt: start.UTC(), EndsAt: sql.NullTime{Time: end.UTC(), Valid: !end.IsZero()}, Location: ledger.NullText(Clean(location, 300)), ExternalID: ledger.NullText(calendar + "|" + uid + "|" + instance)})
	}
	return out, nil
}
