package ledger

import (
	"strings"
	"unicode"
)

// SearchTerms supports short questions without pretending to understand an
// arbitrary request. All remaining terms must match; no model invents facts.
func SearchTerms(query string) []string {
	stop := map[string]bool{"what": true, "when": true, "where": true, "which": true, "do": true, "does": true, "did": true, "i": true, "we": true, "my": true, "our": true, "the": true, "a": true, "an": true, "for": true, "at": true, "to": true, "is": true, "are": true, "about": true, "show": true, "me": true, "please": true}
	var terms []string
	seen := map[string]bool{}
	for _, word := range strings.FieldsFunc(strings.ToLower(query), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if !stop[word] && !seen[word] && len(terms) < 8 {
			terms = append(terms, word)
			seen[word] = true
		}
	}
	return terms
}

type BusinessResult struct {
	Kind   string
	ID     string
	Title  string
	Detail string
	URL    string
}

// SearchBusiness queries source records directly. Each result points to the
// underlying record; this is grounded retrieval, not an LLM-generated answer.
func (s *Store) SearchBusiness(query string) ([]BusinessResult, error) {
	terms := SearchTerms(query)
	if len(terms) == 0 {
		return nil, nil
	}
	q := `SELECT kind,id,title,detail,link FROM (
        SELECT 'job' AS kind,j.id,j.title AS title,
            COALESCE(c.name,'') || ' | ' || COALESCE(p.address,'') || ' | ' || j.status AS detail,
            '/hub' AS link FROM jobs j LEFT JOIN contacts c ON c.id=j.contact_id LEFT JOIN properties p ON p.id=j.property_id
        UNION ALL SELECT 'follow-up',t.id,t.title,
            j.title || ' | ' || COALESCE(c.name,'') || ' | ' || t.status || ' | owner: ' || t.owner || ' | waiting: ' || t.waiting_on,
            '/hub#task-' || t.id FROM commitments t JOIN jobs j ON j.id=t.job_id LEFT JOIN contacts c ON c.id=j.contact_id
        UNION ALL SELECT 'invoice',i.id,COALESCE(i.number,'Invoice'),
            COALESCE(c.name,'') || ' | ' || i.status || ' | $' || printf('%.2f',i.amount_cents/100.0),
            '' FROM invoices i LEFT JOIN contacts c ON c.id=i.contact_id
        UNION ALL SELECT 'email',id,subject,sender || ' | ' || substr(body,1,1000), '/inbox#' || id FROM mail_messages
        UNION ALL SELECT 'contact',id,name,COALESCE(email,'') || ' | ' || COALESCE(phone,''),
            '' FROM contacts
    ) WHERE `
	var args []any
	for i, term := range terms {
		if i > 0 {
			q += " AND "
		}
		q += `instr(lower(title || ' ' || detail),?) > 0`
		args = append(args, term)
	}
	q += ` ORDER BY kind,title,id LIMIT 30`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BusinessResult
	for rows.Next() {
		var r BusinessResult
		if err := rows.Scan(&r.Kind, &r.ID, &r.Title, &r.Detail, &r.URL); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
