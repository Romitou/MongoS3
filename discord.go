package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	colorSuccess = 0x58FF73
	colorWarning = 0xFFB347
	colorFailure = 0xFF5858

	// Discord rejects embeds whose description exceeds 4096 characters.
	maxDescriptionLength = 4096
)

type discordMessage struct {
	Embeds []discordEmbed `json:"embeds"`
}

type discordEmbed struct {
	Title       string         `json:"title"`
	Description string         `json:"description,omitempty"`
	Color       int            `json:"color"`
	Fields      []discordField `json:"fields,omitempty"`
	Footer      *discordFooter `json:"footer,omitempty"`
	Timestamp   string         `json:"timestamp,omitempty"`
}

type discordField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline"`
}

type discordFooter struct {
	Text string `json:"text"`
}

// backupReport sums up a successful backup.
type backupReport struct {
	Name     string
	Duration time.Duration
	Steps    []step

	ArchiveSize  int64
	PreviousSize int64 // 0 when there is no previous backup to compare with
	SizeAlert    bool
	Stats        dumpStats
	Encrypted    bool
	Oplog        bool

	RetentionDays    int
	RetentionSkipped bool
	Pruned           int

	ArchiveURL  string // empty when presigning failed
	LogsURL     string // empty when presigning failed or logs were not uploaded
	LinksExpiry time.Duration
}

// failureReport describes a failed backup.
type failureReport struct {
	Name    string
	Step    string // empty when unknown
	Details string
	Steps   []step // steps done before the failure, the last one failed
}

type discordNotifier struct {
	webhookURL string
	client     *http.Client
}

func newDiscordNotifier(webhookURL string) *discordNotifier {
	return &discordNotifier{webhookURL: webhookURL, client: &http.Client{Timeout: 15 * time.Second}}
}

func (d *discordNotifier) enabled() bool {
	return d.webhookURL != ""
}

func (d *discordNotifier) notifySuccess(ctx context.Context, report backupReport, now time.Time) error {
	return d.send(ctx, successMessage(report, now))
}

func (d *discordNotifier) notifyFailure(ctx context.Context, failure failureReport, now time.Time) error {
	return d.send(ctx, failureMessage(failure, now))
}

func successMessage(r backupReport, now time.Time) discordMessage {
	title, color := "New MongoDB backup", colorSuccess
	description := "A new backup of the database has been performed at " + now.Format("15:04:05") + ". This backup has been sent to the S3 backup server."
	if r.SizeAlert {
		title, color = "⚠️ MongoDB backup smaller than usual", colorWarning
		description = fmt.Sprintf("A backup has been performed at %s, but it is **%.0f%% smaller** than the previous one (%s). Check that no data has been lost: old backups have not been deleted.",
			now.Format("15:04:05"), sizeDrop(r.PreviousSize, r.ArchiveSize), humanSize(r.PreviousSize))
	}

	size := humanSize(r.ArchiveSize)
	if r.PreviousSize > 0 {
		size += fmt.Sprintf(" (%+.1f %%)", -sizeDrop(r.PreviousSize, r.ArchiveSize))
	}

	retention := "Unlimited"
	switch {
	case r.RetentionDays <= 0:
	case r.RetentionSkipped:
		retention = fmt.Sprintf("%d days (skipped)", r.RetentionDays)
	case r.Pruned > 0:
		retention = fmt.Sprintf("%d days (%s deleted)", r.RetentionDays, plural(r.Pruned, "old backup"))
	default:
		retention = fmt.Sprintf("%d days", r.RetentionDays)
	}

	options := []string{}
	if r.Encrypted {
		options = append(options, "🔒 Encrypted (age)")
	} else {
		options = append(options, "🔓 Not encrypted")
	}
	if r.Oplog {
		options = append(options, "🧾 Point-in-time (oplog)")
	}

	fields := []discordField{
		{Name: "⌛ Execution time", Value: r.Duration.Round(time.Second).String(), Inline: true},
		{Name: "📄 Backup size", Value: size, Inline: true},
		{Name: "✅ Retention period", Value: retention, Inline: true},
	}
	if r.Stats.Collections > 0 {
		fields = append(fields, discordField{
			Name: "🗃️ Content",
			Value: fmt.Sprintf("%s, %s\n%s",
				plural(r.Stats.Databases, "database"), plural(r.Stats.Collections, "collection"), plural64(r.Stats.Documents, "document")),
			Inline: true,
		})
	}
	fields = append(fields,
		discordField{Name: "🛡️ Options", Value: strings.Join(options, "\n"), Inline: true},
		discordField{Name: "⏱️ Steps", Value: formatSteps(r.Steps, false), Inline: true},
	)
	if r.LogsURL != "" {
		fields = append(fields, discordField{Name: "📜 Backup logs", Value: "[Click to open the generated logs](" + r.LogsURL + ")", Inline: true})
	}
	if r.ArchiveURL != "" {
		fields = append(fields, discordField{
			Name:   "📥 Download link",
			Value:  fmt.Sprintf("[Click to download the backup archive](%s)\nValid for %s", r.ArchiveURL, humanDuration(r.LinksExpiry)),
			Inline: true,
		})
	}

	return discordMessage{Embeds: []discordEmbed{{
		Title:       title,
		Description: description,
		Color:       color,
		Fields:      fields,
		Footer:      footer(r.Name),
		Timestamp:   now.Format(time.RFC3339),
	}}}
}

func failureMessage(f failureReport, now time.Time) discordMessage {
	intro := "A backup attempt of the MongoDB database has been performed at " + now.Format("15:04:05") + ". An error was encountered during the backup process:\n"
	const fence = "```"
	budget := maxDescriptionLength - len([]rune(intro)) - 2*len(fence) - 2
	// A fence inside the details would close the code block early.
	details := strings.ReplaceAll(f.Details, fence, "'''")

	var fields []discordField
	if f.Step != "" {
		fields = append(fields, discordField{Name: "💥 Failed step", Value: f.Step, Inline: true})
	}
	if len(f.Steps) > 0 {
		fields = append(fields, discordField{Name: "⏱️ Steps", Value: formatSteps(f.Steps, f.Step != ""), Inline: true})
	}

	return discordMessage{Embeds: []discordEmbed{{
		Title:       "Failed MongoDB backup",
		Description: intro + fence + "\n" + truncateHead(details, budget) + "\n" + fence,
		Color:       colorFailure,
		Fields:      fields,
		Footer:      footer(f.Name),
		Timestamp:   now.Format(time.RFC3339),
	}}}
}

func footer(name string) *discordFooter {
	if name == "" {
		return nil
	}
	return &discordFooter{Text: "🖥️ " + name}
}

// formatSteps lists the steps with their duration; when lastFailed is true
// the last step is marked as failed.
func formatSteps(steps []step, lastFailed bool) string {
	lines := make([]string, len(steps))
	for i, s := range steps {
		mark := "✔️"
		if lastFailed && i == len(steps)-1 {
			mark = "❌"
		}
		lines[i] = fmt.Sprintf("%s %s: %s", mark, s.Name, roundDuration(s.Duration))
	}
	return strings.Join(lines, "\n")
}

func (d *discordNotifier) send(ctx context.Context, message discordMessage) error {
	if !d.enabled() {
		return nil
	}
	body, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode Discord message: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create Discord request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("send Discord message: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("Discord webhook answered %s: %s", resp.Status, respBody)
	}
	return nil
}

// truncateHead keeps the last max characters of s: the end of a log is where
// the actual error usually is.
func truncateHead(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	const marker = "…\n"
	keep := max - len([]rune(marker))
	if keep < 0 {
		keep = 0
	}
	return marker + string(runes[len(runes)-keep:])
}

func humanSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

func humanDuration(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		return plural(int(d/(24*time.Hour)), "day")
	}
	return d.String()
}

// roundDuration keeps durations readable: 1.2s, 3m4s...
func roundDuration(d time.Duration) string {
	if d < time.Minute {
		return d.Round(100 * time.Millisecond).String()
	}
	return d.Round(time.Second).String()
}

func plural(n int, word string) string {
	return plural64(int64(n), word)
}

func plural64(n int64, word string) string {
	if n != 1 {
		word += "s"
	}
	return formatCount(n) + " " + word
}

// formatCount adds thousands separators: 1234567 -> 1,234,567.
func formatCount(n int64) string {
	s := strconv.FormatInt(n, 10)
	start := 0
	if n < 0 {
		start = 1
	}
	for i := len(s) - 3; i > start; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
