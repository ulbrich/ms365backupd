package notify

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ulbrich/ms365backupd/internal/status"
)

// HumanBytes formats a byte count.
func HumanBytes(n uint64) string {
	const u = 1024
	if n < u {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(u), 0
	for x := n / u; x >= u; x /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// host is a variable so tests get a fixed name.
var host = func() string {
	h, _ := os.Hostname()
	return h
}

// FailureMessage formats an alert for a failed (or partial) backup.
func FailureMessage(r *status.RunResult, attempts int, lastSuccess *time.Time) Message {
	subj := fmt.Sprintf("[ms365backupd] Backup %s on %s", strings.ToUpper(r.Status), host())
	var b strings.Builder
	fmt.Fprintf(&b, "The Microsoft 365 backup did not complete successfully.\n\n")
	fmt.Fprintf(&b, "Status:       %s\n", r.Status)
	fmt.Fprintf(&b, "Error:        %s\n", r.Error)
	fmt.Fprintf(&b, "Trigger:      %s\n", r.Trigger)
	fmt.Fprintf(&b, "Started:      %s\n", r.Started.Format(time.RFC1123))
	fmt.Fprintf(&b, "Duration:     %s\n", r.Duration().Round(time.Second))
	if attempts > 1 {
		fmt.Fprintf(&b, "Attempts:     %d (all failed)\n", attempts)
	}
	if r.Account != "" {
		fmt.Fprintf(&b, "Account:      %s\n", r.Account)
	}
	if r.DiskID != "" {
		fmt.Fprintf(&b, "Disk:         %s (%s free of %s)\n", r.DiskID, HumanBytes(r.DiskFree), HumanBytes(r.DiskTotal))
	}
	if lastSuccess != nil {
		fmt.Fprintf(&b, "Last success: %s (%s ago)\n", lastSuccess.Format(time.RFC1123), time.Since(*lastSuccess).Round(time.Minute))
	} else {
		fmt.Fprintf(&b, "Last success: never\n")
	}
	if len(r.ErrorSamples) > 0 {
		fmt.Fprintf(&b, "\nErrors (first %d):\n", len(r.ErrorSamples))
		for _, s := range r.ErrorSamples {
			fmt.Fprintf(&b, "  - %s\n", s)
		}
	}
	fmt.Fprintf(&b, "\nWork done before the failure:\n")
	writeCounters(&b, r.Services, r.Totals)
	fmt.Fprintf(&b, "\nCheck the logs with `journalctl -u ms365backupd@$USER` or run `ms365backupd tui`.\n")
	return Message{Subject: subj, Text: b.String(), HTML: failureHTML(r, attempts, lastSuccess)}
}

func failureHTML(r *status.RunResult, attempts int, lastSuccess *time.Time) string {
	d := htmlDoc{
		Title:    "Backup did not complete",
		Subtitle: host() + " · " + r.Started.Format("Mon Jan 2 2006, 15:04 MST"),
		Badge:    strings.ToUpper(r.Status),
		Tone:     toneBad,
	}
	if r.Status == status.StatusPartial {
		d.Tone = toneWarn
	}
	if r.Error != "" {
		d.Badge += ": " + r.Error
	}
	det := []kv{
		{K: "Trigger", V: r.Trigger},
		{K: "Started", V: r.Started.Format(time.RFC1123)},
		{K: "Duration", V: r.Duration().Round(time.Second).String()},
	}
	if attempts > 1 {
		det = append(det, kv{K: "Attempts", V: fmt.Sprintf("%d (all failed)", attempts)})
	}
	if r.Account != "" {
		det = append(det, kv{K: "Account", V: r.Account})
	}
	if r.DiskID != "" {
		det = append(det, kv{K: "Disk", V: fmt.Sprintf("%s free of %s", HumanBytes(r.DiskFree), HumanBytes(r.DiskTotal))},
			kv{K: "Disk id", V: r.DiskID, Mono: true})
	}
	if lastSuccess != nil {
		det = append(det, kv{K: "Last success", V: fmt.Sprintf("%s (%s ago)", lastSuccess.Format(time.RFC1123), time.Since(*lastSuccess).Round(time.Minute))})
	} else {
		det = append(det, kv{K: "Last success", V: "never", Warn: true})
	}
	d.Sections = append(d.Sections, htmlSection{Heading: "Details", KV: det})
	if len(r.ErrorSamples) > 0 {
		d.Sections = append(d.Sections, htmlSection{Heading: fmt.Sprintf("Errors (first %d)", len(r.ErrorSamples)), List: r.ErrorSamples})
	}
	d.Sections = append(d.Sections, htmlSection{Heading: "Work done before the failure", Table: countersTable(r.Services, r.Totals)})
	d.Footer = []htmlPart{{Text: "Check the logs with "}, {Text: "journalctl -u ms365backupd -e", Code: true},
		{Text: " or run "}, {Text: "ms365backupd tui", Code: true}, {Text: "."}}
	return renderHTML(d)
}

// WeeklyMessage formats the weekly statistics.
func WeeklyMessage(runs []status.RunResult, from, to time.Time, disk status.DiskState, dataSize uint64, files int64) Message {
	var ok, partial, failed int
	var tot status.Counters
	per := map[string]status.Counters{}
	var lastOK *status.RunResult
	var dur time.Duration
	for i := range runs {
		r := runs[i]
		switch r.Status {
		case status.StatusSuccess:
			ok++
			lastOK = &runs[i]
		case status.StatusPartial:
			partial++
		default:
			failed++
		}
		tot.Add(r.Totals)
		dur += r.Duration()
		for k, c := range r.Services {
			x := per[k]
			x.Add(c)
			per[k] = x
		}
	}
	state := "OK"
	if failed > 0 || partial > 0 {
		state = "ATTENTION"
	}
	if ok == 0 {
		state = "NO SUCCESSFUL BACKUP"
	}
	subj := fmt.Sprintf("[ms365backupd] Weekly report %s – %s (%s)", from.Format("Jan 2"), to.Format("Jan 2"), state)
	var b strings.Builder
	fmt.Fprintf(&b, "Microsoft 365 backup – weekly report for %s\n", host())
	fmt.Fprintf(&b, "Period: %s – %s\n\n", from.Format("Mon Jan 2 2006"), to.Format("Mon Jan 2 2006"))
	fmt.Fprintf(&b, "Runs:          %d (%d successful, %d partial, %d failed)\n", len(runs), ok, partial, failed)
	if len(runs) > 0 {
		fmt.Fprintf(&b, "Total runtime: %s\n", dur.Round(time.Second))
	}
	if lastOK != nil {
		fmt.Fprintf(&b, "Last success:  %s\n", lastOK.Finished.Format(time.RFC1123))
	} else {
		fmt.Fprintf(&b, "Last success:  none this week!\n")
	}
	fmt.Fprintf(&b, "\nBackup disk:   %s\n", disk.Path)
	if disk.Present {
		fmt.Fprintf(&b, "  disk id:     %s\n", disk.DiskID)
		fmt.Fprintf(&b, "  free:        %s of %s (%.0f%% used)\n", HumanBytes(disk.Free), HumanBytes(disk.Total), usedPct(disk))
		if dataSize > 0 {
			fmt.Fprintf(&b, "  backup data: %s in %d files\n", HumanBytes(dataSize), files)
		}
	} else {
		fmt.Fprintf(&b, "  NOT CONNECTED: %s\n", disk.Reason)
	}
	fmt.Fprintf(&b, "\nChanges this week:\n")
	writeCounters(&b, per, tot)
	if failed+partial > 0 {
		fmt.Fprintf(&b, "\nProblems:\n")
		for _, r := range runs {
			if r.Status != status.StatusSuccess {
				fmt.Fprintf(&b, "  %s  %-7s %s\n", r.Started.Format("Mon 02 15:04"), r.Status, r.Error)
			}
		}
	}

	// HTML version, same content.
	d := htmlDoc{
		Title:    "Weekly backup report",
		Subtitle: host() + " · " + from.Format("Mon Jan 2") + " – " + to.Format("Mon Jan 2 2006"),
		Tone:     toneOK,
		Badge:    fmt.Sprintf("OK: %d of %d runs successful", ok, len(runs)),
	}
	switch state {
	case "ATTENTION":
		d.Tone = toneWarn
		d.Badge = fmt.Sprintf("ATTENTION: %d partial, %d failed of %d runs", partial, failed, len(runs))
	case "NO SUCCESSFUL BACKUP":
		d.Tone = toneBad
		d.Badge = "NO SUCCESSFUL BACKUP this week"
	}
	runKV := []kv{{K: "Runs", V: fmt.Sprintf("%d (%d successful, %d partial, %d failed)", len(runs), ok, partial, failed)}}
	if len(runs) > 0 {
		runKV = append(runKV, kv{K: "Total runtime", V: dur.Round(time.Second).String()})
	}
	if lastOK != nil {
		runKV = append(runKV, kv{K: "Last success", V: lastOK.Finished.Format(time.RFC1123)})
	} else {
		runKV = append(runKV, kv{K: "Last success", V: "none this week!", Warn: true})
	}
	d.Sections = append(d.Sections, htmlSection{Heading: "Runs", KV: runKV})
	diskKV := []kv{{K: "Path", V: disk.Path, Mono: true}}
	if disk.Present {
		diskKV = append(diskKV,
			kv{K: "Free", V: fmt.Sprintf("%s of %s (%.0f%% used)", HumanBytes(disk.Free), HumanBytes(disk.Total), usedPct(disk))})
		if dataSize > 0 {
			diskKV = append(diskKV, kv{K: "Backup data", V: fmt.Sprintf("%s in %d files", HumanBytes(dataSize), files)})
		}
		diskKV = append(diskKV, kv{K: "Disk id", V: disk.DiskID, Mono: true})
	} else {
		diskKV = append(diskKV, kv{K: "Status", V: "NOT CONNECTED: " + disk.Reason, Warn: true})
	}
	d.Sections = append(d.Sections, htmlSection{Heading: "Backup disk", KV: diskKV},
		htmlSection{Heading: "Changes this week", Table: countersTable(per, tot)})
	if failed+partial > 0 {
		t := &htmlTable{Head: []string{"When", "Status", "Error"}}
		for _, r := range runs {
			if r.Status != status.StatusSuccess {
				t.Rows = append(t.Rows, []string{r.Started.Format("Mon 02 15:04"), r.Status, r.Error})
			}
		}
		d.Sections = append(d.Sections, htmlSection{Heading: "Problems", Table: t})
	}
	return Message{Subject: subj, Text: b.String(), HTML: renderHTML(d)}
}

// countersTable is the HTML counterpart of writeCounters.
func countersTable(per map[string]status.Counters, tot status.Counters) *htmlTable {
	keys := make([]string, 0, len(per))
	for k := range per {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t := &htmlTable{Head: []string{"Service", "New", "Updated", "Deleted", "Moved", "Skipped", "Errors", "Download"}, Right: 1, Total: true}
	row := func(name string, c status.Counters) {
		t.Rows = append(t.Rows, []string{name, fmt.Sprint(c.New), fmt.Sprint(c.Updated), fmt.Sprint(c.Deleted), fmt.Sprint(c.Moved),
			fmt.Sprint(c.Skipped), fmt.Sprint(c.Errors), HumanBytes(uint64(c.Bytes))})
	}
	for _, k := range keys {
		row(k, per[k])
	}
	row("Total", tot)
	return t
}

// TestMessage is the message of `ms365backupd test-email`.
func TestMessage() Message {
	h := host()
	return Message{
		Subject: "[ms365backupd] Test notification from " + h,
		Text:    "This is a test message from ms365backupd.\n\nFailure alerts and weekly reports will be delivered like this one.\n",
		HTML: renderHTML(htmlDoc{
			Title:    "Test notification",
			Subtitle: h,
			Badge:    "OK: e-mail delivery works",
			Tone:     toneOK,
			Intro:    "This is a test message from ms365backupd. Failure alerts and weekly reports will be delivered like this one.",
		}),
	}
}

func usedPct(d status.DiskState) float64 {
	if d.Total == 0 {
		return 0
	}
	return 100 * float64(d.Total-d.Free) / float64(d.Total)
}

func writeCounters(b *strings.Builder, per map[string]status.Counters, tot status.Counters) {
	keys := make([]string, 0, len(per))
	for k := range per {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(b, "  %-11s %8s %8s %8s %8s %8s %8s %10s\n", "service", "new", "updated", "deleted", "moved", "skipped", "errors", "download")
	row := func(name string, c status.Counters) {
		fmt.Fprintf(b, "  %-11s %8d %8d %8d %8d %8d %8d %10s\n", name, c.New, c.Updated, c.Deleted, c.Moved, c.Skipped, c.Errors, HumanBytes(uint64(c.Bytes)))
	}
	for _, k := range keys {
		row(k, per[k])
	}
	row("TOTAL", tot)
}
