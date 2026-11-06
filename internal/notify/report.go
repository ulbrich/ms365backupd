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

func host() string {
	h, _ := os.Hostname()
	return h
}

// FailureMessage formats an alert for a failed (or partial) backup.
func FailureMessage(r *status.RunResult, attempts int, lastSuccess *time.Time) (string, string) {
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
	return subj, b.String()
}

// WeeklyMessage formats the weekly statistics.
func WeeklyMessage(runs []status.RunResult, from, to time.Time, disk status.DiskState, dataSize uint64, files int64) (string, string) {
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
	return subj, b.String()
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
