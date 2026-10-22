package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"filippo.io/age"
	"github.com/joho/godotenv"
)

// Maximum time given to the failure notification once the backup has failed
// (the main context may already be cancelled at that point).
const notifyTimeout = 30 * time.Second

func main() {
	// The .env file is optional: variables can also come from the real
	// environment (systemd unit, Docker, CI...).
	if err := godotenv.Load(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Fatalf("Error loading .env file: %v", err)
	}

	start := time.Now()
	notifier := newDiscordNotifier(os.Getenv("DISCORD_URL"))

	cfg, err := loadConfig(os.Getenv)
	if cfg.Name == "" {
		cfg.Name, _ = os.Hostname()
	}
	if err != nil {
		fail(notifier, cfg.Name, nil, &stepError{Step: "Configuration", Err: err}, "")
	}
	if !notifier.enabled() {
		log.Println("DISCORD_URL is not set, Discord notifications are disabled")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	report, dumpLogs, err := run(ctx, cfg, start)
	if err != nil {
		stop()
		fail(notifier, cfg.Name, report.Steps, err, dumpLogs)
	}

	log.Printf("Backup done in %s (%s)", report.Duration.Round(time.Millisecond), humanSize(report.ArchiveSize))
	if notifier.enabled() {
		log.Println("Sending a notification to Discord...")
		if err := notifier.notifySuccess(ctx, report, time.Now()); err != nil {
			log.Printf("Warning: %v", err)
		}
	}
}

type step struct {
	Name     string
	Duration time.Duration
}

// stepError tells which step of the backup failed.
type stepError struct {
	Step string
	Err  error
}

func (e *stepError) Error() string { return e.Step + ": " + e.Err.Error() }
func (e *stepError) Unwrap() error { return e.Err }

// run performs the backup. The report (with the steps done so far) and the
// mongodump logs are returned even on failure so they can be reported.
func run(ctx context.Context, cfg *config, start time.Time) (backupReport, string, error) {
	report := backupReport{
		Name:          cfg.Name,
		RetentionDays: cfg.RetentionDays,
		LinksExpiry:   cfg.PresignExpiry,
		Encrypted:     len(cfg.Recipients) > 0,
		Oplog:         cfg.Oplog,
	}
	// timed runs one step of the backup and records its duration.
	timed := func(name string, fn func() error) error {
		log.Printf("%s...", name)
		stepStart := time.Now()
		err := fn()
		report.Steps = append(report.Steps, step{Name: name, Duration: time.Since(stepStart)})
		if err != nil {
			return &stepError{Step: name, Err: err}
		}
		return nil
	}

	store, err := newStorage(cfg)
	if err != nil {
		return report, "", &stepError{Step: "S3 check", Err: err}
	}

	// Fail fast, before a potentially long dump, if S3 is unreachable.
	if err := timed("S3 check", func() error { return store.checkBucket(ctx) }); err != nil {
		return report, "", err
	}

	base := cfg.S3Prefix + backupObjectPrefix + start.UTC().Format("2006-01-02T150405Z")
	extension := ".archive"
	if cfg.Gzip {
		extension += ".gz"
	}
	if report.Encrypted {
		extension += ".age"
	}
	archiveKey := base + extension
	logsKey := base + ".log"

	var dumpLogs string
	err = timed("Dump & upload", func() error {
		var err error
		dumpLogs, report.ArchiveSize, err = dumpToS3(ctx, cfg, store, archiveKey)
		return err
	})
	report.Stats = parseDumpStats(dumpLogs)
	if err != nil {
		return report, dumpLogs, err
	}
	log.Printf("Archive %q uploaded", archiveKey)

	// From here on, the backup is safely stored: the remaining steps only
	// emit warnings instead of failing the whole backup.
	logsUploaded := timed("Logs upload", func() error {
		_, err := store.uploadText(ctx, logsKey, dumpLogs)
		return err
	}) == nil
	if !logsUploaded {
		log.Printf("Warning: logs upload failed")
	}

	if err := timed("Retention", func() error { return applyRetention(ctx, cfg, store, base, extension, start, &report) }); err != nil {
		log.Printf("Warning: %v", err)
	}

	_ = timed("Links", func() error {
		if u, err := store.presign(ctx, archiveKey); err != nil {
			log.Printf("Warning: %v", err)
		} else {
			report.ArchiveURL = u.String()
		}
		if logsUploaded {
			if u, err := store.presign(ctx, logsKey); err != nil {
				log.Printf("Warning: %v", err)
			} else {
				report.LogsURL = u.String()
			}
		}
		return nil
	})

	report.Duration = time.Since(start)
	return report, dumpLogs, nil
}

// dumpToS3 streams mongodump's output, encrypted if needed, to S3 without
// touching the disk. It returns the mongodump logs and the archive size.
func dumpToS3(ctx context.Context, cfg *config, store *storage, key string) (string, int64, error) {
	// Only the mongodump config file (holding the URI) is written to disk.
	workDir, err := os.MkdirTemp("", "mongos3-")
	if err != nil {
		return "", 0, fmt.Errorf("create temporary directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	dumpCtx, cancelDump := context.WithCancel(ctx)
	defer cancelDump()

	type dumpResult struct {
		logs string
		err  error
	}
	results := make(chan dumpResult, 1)
	reader, writer := io.Pipe()
	// Set before the dump error reaches the upload through the pipe.
	var dumpFailed atomic.Bool

	go func() {
		var out io.WriteCloser = nopWriteCloser{writer}
		if len(cfg.Recipients) > 0 {
			encrypted, err := age.Encrypt(writer, cfg.Recipients...)
			if err != nil {
				err = fmt.Errorf("encryption: %w", err)
				writer.CloseWithError(err)
				results <- dumpResult{err: err}
				return
			}
			out = encrypted
		}
		logs, err := mongoDump(dumpCtx, cfg, workDir, out)
		if err == nil {
			// Writes the final encrypted chunk.
			if err = out.Close(); err != nil {
				err = fmt.Errorf("encryption: %w", err)
			}
		}
		if err != nil {
			dumpFailed.Store(true)
		}
		// A nil error ends the upload normally; any other aborts it, so a
		// failed dump is never stored as a truncated archive.
		writer.CloseWithError(err)
		results <- dumpResult{logs: logs, err: err}
	}()

	info, uploadErr := store.uploadStream(ctx, key, reader)
	// When both fail, report the root cause: either the dump failed and
	// aborted the upload, or the upload failed and we stop the dump.
	dumpFailedFirst := dumpFailed.Load()
	if uploadErr != nil {
		// Stop mongodump, nobody reads its output anymore.
		cancelDump()
		reader.CloseWithError(uploadErr)
	}
	result := <-results

	if result.err != nil && (uploadErr == nil || dumpFailedFirst) {
		return result.logs, 0, result.err
	}
	if uploadErr != nil {
		return result.logs, 0, uploadErr
	}
	return result.logs, info.Size, nil
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// applyRetention compares the new archive with the previous one and deletes
// the expired backups, unless the new archive looks suspiciously small.
func applyRetention(ctx context.Context, cfg *config, store *storage, base, extension string, now time.Time, report *backupReport) error {
	objects, err := store.listBackups(ctx)
	if err != nil {
		return err
	}
	sets := groupBackups(objects)

	if previous := previousArchive(sets, base, extension); previous != nil {
		report.PreviousSize = previous.Size
		drop := sizeDrop(previous.Size, report.ArchiveSize)
		if cfg.SizeAlertPercent > 0 && drop >= float64(cfg.SizeAlertPercent) {
			report.SizeAlert = true
			log.Printf("Warning: the archive is %.0f%% smaller than the previous one (%s)", drop, humanSize(previous.Size))
		}
	}

	if cfg.RetentionDays <= 0 {
		return nil
	}
	if report.SizeAlert {
		report.RetentionSkipped = true
		log.Println("Retention skipped because of the size drop")
		return nil
	}

	cutoff := now.AddDate(0, 0, -cfg.RetentionDays)
	for _, set := range expiredBackups(sets, cutoff, cfg.KeepMin) {
		for _, object := range set.Objects {
			if err := store.remove(ctx, object.Key); err != nil {
				return err
			}
		}
		report.Pruned++
		log.Printf("Deleted old backup %q", set.Base)
	}
	return nil
}

// fail reports the error to Discord and exits with a non-zero status.
func fail(notifier *discordNotifier, name string, steps []step, err error, dumpLogs string) {
	log.Printf("Backup failed: %v", err)

	failure := failureReport{Name: name, Steps: steps, Details: err.Error()}
	var se *stepError
	if errors.As(err, &se) {
		failure.Step = se.Step
		failure.Details = se.Err.Error()
	}
	if dumpLogs != "" {
		failure.Details += "\n\n" + dumpLogs
	}
	if notifier.enabled() {
		ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
		defer cancel()
		if notifyErr := notifier.notifyFailure(ctx, failure, time.Now()); notifyErr != nil {
			log.Printf("Warning: %v", notifyErr)
		}
	}
	os.Exit(1)
}
