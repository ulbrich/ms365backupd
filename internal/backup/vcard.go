package backup

import (
	"errors"
	"os"
	"strings"
)

// vcard renders a Graph contact as vCard 3.0 for easy import into any address book.
func vcard(o map[string]any) []byte {
	var b strings.Builder
	line := func(k, v string) {
		if v != "" {
			b.WriteString(k + ":" + v + "\r\n")
		}
	}
	b.WriteString("BEGIN:VCARD\r\nVERSION:3.0\r\n")
	fn := str(o, "displayName")
	if fn == "" {
		fn = strings.TrimSpace(str(o, "givenName") + " " + str(o, "surname"))
	}
	b.WriteString("FN:" + esc(fn) + "\r\n")
	b.WriteString("N:" + esc(str(o, "surname")) + ";" + esc(str(o, "givenName")) + ";" + esc(str(o, "middleName")) + ";" +
		esc(str(o, "title")) + ";" + esc(str(o, "generation")) + "\r\n")
	line("NICKNAME", esc(str(o, "nickName")))
	if org := str(o, "companyName"); org != "" {
		line("ORG", esc(org)+";"+esc(str(o, "department")))
	}
	line("TITLE", esc(str(o, "jobTitle")))
	if em, ok := o["emailAddresses"].([]any); ok {
		for _, x := range em {
			if m, ok := x.(map[string]any); ok {
				line("EMAIL;TYPE=INTERNET", esc(str(m, "address")))
			}
		}
	}
	phones := func(k, typ string) {
		if arr, ok := o[k].([]any); ok {
			for _, x := range arr {
				if s, ok := x.(string); ok {
					line("TEL;TYPE="+typ, esc(s))
				}
			}
		}
	}
	phones("businessPhones", "WORK,VOICE")
	phones("homePhones", "HOME,VOICE")
	line("TEL;TYPE=CELL", esc(str(o, "mobilePhone")))
	for k, typ := range map[string]string{"homeAddress": "HOME", "businessAddress": "WORK", "otherAddress": "OTHER"} {
		if a, ok := o[k].(map[string]any); ok && len(a) > 0 {
			v := ";;" + esc(str(a, "street")) + ";" + esc(str(a, "city")) + ";" + esc(str(a, "state")) + ";" +
				esc(str(a, "postalCode")) + ";" + esc(str(a, "countryOrRegion"))
			if strings.Trim(v, ";") != "" {
				line("ADR;TYPE="+typ, v)
			}
		}
	}
	if bd := str(o, "birthday"); len(bd) >= 10 {
		line("BDAY", bd[:10])
	}
	if urls, ok := o["businessHomePage"].(string); ok {
		line("URL", esc(urls))
	}
	line("NOTE", esc(str(o, "personalNotes")))
	line("UID", esc(str(o, "id")))
	b.WriteString("END:VCARD\r\n")
	return []byte(b.String())
}

func esc(s string) string {
	r := strings.NewReplacer(`\`, `\\`, ",", `\,`, ";", `\;`, "\r\n", `\n`, "\n", `\n`)
	return r.Replace(s)
}

func removeFile(p string) error {
	err := os.Remove(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
