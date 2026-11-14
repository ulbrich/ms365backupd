package notify

import (
	"bytes"
	"html/template"
)

// The HTML version of the mails: one centred card, inline styles only (many mail clients
// drop <style> blocks), tables for layout, light colours that also survive dark-mode
// inversion. The text version stays the reference; this mirrors its content.

type tone string

const (
	toneOK   tone = "ok"
	toneWarn tone = "warn"
	toneBad  tone = "bad"
)

type htmlDoc struct {
	Title    string
	Subtitle string
	Badge    string
	Tone     tone
	Intro    string
	Sections []htmlSection
	Footer   []htmlPart
}

type htmlSection struct {
	Heading string
	KV      []kv
	Table   *htmlTable
	List    []string // monospace lines
}

type kv struct {
	K, V string
	Warn bool // highlight the value
	Mono bool
}

type htmlTable struct {
	Head  []string
	Rows  [][]string
	Right int  // columns from this index on are right-aligned numbers (0: none)
	Total bool // the last row is a total
}

// htmlPart is a piece of running text; Code parts are set in monospace.
type htmlPart struct {
	Text string
	Code bool
}

var toneStyles = map[tone]template.CSS{
	toneOK:   "background:#dafbe1;color:#116329;border:1px solid #aceebb",
	toneWarn: "background:#fff8c5;color:#7d4e00;border:1px solid #f0d675",
	toneBad:  "background:#ffebe9;color:#a40e26;border:1px solid #ffcecb",
}

var htmlTmpl = template.Must(template.New("mail").Funcs(template.FuncMap{
	"tone":  func(t tone) template.CSS { return toneStyles[t] },
	"right": func(t *htmlTable, i int) bool { return t.Right > 0 && i >= t.Right },
	"last":  func(i int, rows [][]string) bool { return i == len(rows)-1 },
	"zero":  func(s string) bool { return s == "0" || s == "0 B" },
}).Parse(`<!DOCTYPE html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="color-scheme" content="light only"><title>{{.Title}}</title></head>
<body style="margin:0;padding:0;background:#f6f8fa">
<div style="max-width:680px;margin:0 auto;padding:24px 12px;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Helvetica,Arial,sans-serif;font-size:14px;line-height:1.5;color:#1f2328">
<div style="background:#ffffff;border:1px solid #d0d7de;border-radius:8px;padding:20px 22px">
<div style="font-size:11px;color:#656d76;text-transform:uppercase;letter-spacing:0.08em">ms365backupd</div>
<h1 style="margin:2px 0 0;font-size:20px;line-height:1.3;font-weight:600">{{.Title}}</h1>
{{- with .Subtitle}}
<div style="color:#656d76">{{.}}</div>
{{- end}}
{{- with .Badge}}
<div style="margin-top:14px;padding:8px 12px;border-radius:6px;font-weight:600;{{tone $.Tone}}">{{.}}</div>
{{- end}}
{{- with .Intro}}
<p style="margin:14px 0 0">{{.}}</p>
{{- end}}
{{- range .Sections}}
<h2 style="margin:22px 0 6px;padding-bottom:4px;border-bottom:1px solid #d0d7de;font-size:13px;font-weight:600;color:#424a53;text-transform:uppercase;letter-spacing:0.05em">{{.Heading}}</h2>
{{- with .KV}}
<table role="presentation" cellpadding="0" cellspacing="0" style="border-collapse:collapse;width:100%">
{{- range .}}
<tr><td style="padding:3px 12px 3px 0;color:#656d76;white-space:nowrap;vertical-align:top;width:120px">{{.K}}</td><td style="padding:3px 0;{{if .Warn}}color:#a40e26;font-weight:600;{{end}}{{if .Mono}}font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:13px;word-break:break-all;{{end}}">{{.V}}</td></tr>
{{- end}}
</table>
{{- end}}
{{- with .Table}}{{$t := .}}
<div style="overflow-x:auto">
<table cellpadding="0" cellspacing="0" style="border-collapse:collapse;width:100%;font-size:13px">
<tr>{{range $i, $h := .Head}}<th style="padding:5px 8px;border-bottom:1px solid #d0d7de;color:#656d76;font-weight:600;text-align:{{if right $t $i}}right{{else}}left{{end}};white-space:nowrap">{{$h}}</th>{{end}}</tr>
{{- range $r, $row := .Rows}}{{$total := and $t.Total (last $r $t.Rows)}}
<tr{{if $total}} style="background:#f6f8fa;font-weight:600"{{end}}>{{range $i, $c := $row}}<td style="padding:5px 8px;border-bottom:1px solid #eaeef2;{{if right $t $i}}text-align:right;white-space:nowrap;font-variant-numeric:tabular-nums;{{if and (zero $c) (not $total)}}color:#8c959f;{{end}}{{else if eq $i 0}}white-space:nowrap;{{end}}">{{$c}}</td>{{end}}</tr>
{{- end}}
</table>
</div>
{{- end}}
{{- with .List}}
<ul style="margin:4px 0 0;padding-left:18px">
{{- range .}}
<li style="margin:2px 0;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12px;word-break:break-all">{{.}}</li>
{{- end}}
</ul>
{{- end}}
{{- end}}
{{- with .Footer}}
<p style="margin:22px 0 0;color:#656d76;font-size:13px">{{range .}}{{if .Code}}<code style="font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12px;background:#f6f8fa;border:1px solid #eaeef2;border-radius:4px;padding:1px 4px">{{.Text}}</code>{{else}}{{.Text}}{{end}}{{end}}</p>
{{- end}}
</div>
</div>
</body></html>
`))

func renderHTML(d htmlDoc) string {
	var b bytes.Buffer
	if err := htmlTmpl.Execute(&b, d); err != nil {
		return "" // a template bug must not stop the alert; the text version is still sent
	}
	return b.String()
}
