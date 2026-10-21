package main

// DownloadStats counts completed saves, verified existing tiles and tile errors.
// Queued SQLite writes only count as saved after the database write succeeds.
type DownloadStats struct {
	Saved   int64
	Skipped int64
	Failed  int64
}

func (task *Task) Stats() DownloadStats {
	return DownloadStats{Saved: task.saved.Load(), Skipped: task.skipped.Load(), Failed: task.failed.Load()}
}

func (task *Task) tileFailure(err error) {
	task.failed.Add(1)
	task.recordError(err)
}
