package notify

import (
	"time"

	"github.com/ulbrich/ms365backupd/internal/status"
)

var (
	fxT0 = time.Date(2026, 11, 9, 2, 0, 0, 0, time.UTC)
	fxT1 = fxT0.AddDate(0, 0, 7)
)

func fxRuns() []status.RunResult {
	c := func(n, u, d, m, s, e, b int64) status.Counters {
		return status.Counters{New: n, Updated: u, Deleted: d, Moved: m, Skipped: s, Errors: e, Bytes: b}
	}
	return []status.RunResult{
		{Trigger: "schedule", Started: fxT0, Finished: fxT0.Add(40 * time.Minute), Status: status.StatusSuccess,
			Services: map[string]status.Counters{"onedrive": c(12, 3, 1, 0, 0, 0, 52_000_000), "mail": c(140, 0, 2, 1, 0, 0, 9_100_000)},
			Totals:   c(152, 3, 3, 1, 0, 0, 61_100_000)},
		{Trigger: "disk-connected", Started: fxT0.Add(26 * time.Hour), Finished: fxT0.Add(27 * time.Hour), Status: status.StatusPartial,
			Error:    "2 items failed <will retry>",
			Services: map[string]status.Counters{"sharepoint": c(3398, 0, 0, 0, 4, 2, 5_900_000_000)},
			Totals:   c(3398, 0, 0, 0, 4, 2, 5_900_000_000)},
		{Trigger: "schedule", Started: fxT0.Add(50 * time.Hour), Finished: fxT0.Add(50*time.Hour + time.Minute), Status: status.StatusFailed,
			Error: "target is not a mount point (disk not connected?)"},
	}
}

func fxFailure() *status.RunResult {
	r := fxRuns()[1]
	r.Status = status.StatusFailed
	r.Account = "admin@contoso.onmicrosoft.com"
	r.DiskID = "08be704b6bc03b2087bfc7e050ad6123"
	r.DiskFree, r.DiskTotal = 700<<30, 931<<30
	r.ErrorSamples = []string{"sharepoint/Site/Docs/a & b.pdf: 503 Service Unavailable", "onedrive/x.docx: hash mismatch"}
	return &r
}

var fxDisk = status.DiskState{Path: "/mnt/backup", Present: true, DiskID: "08be704b6bc03b2087bfc7e050ad6123", Free: 700 << 30, Total: 931 << 30}
