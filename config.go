package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"filippo.io/age"
	"filippo.io/age/agessh"
)

// maxPresignExpiry is the longest validity allowed by S3 signature v4.
const maxPresignExpiry = 7 * 24 * time.Hour

// S3 multipart limits: parts of 5 MiB to 5 GiB.
const (
	minPartSizeMiB = 5
	maxPartSizeMiB = 5 << 10
)

type config struct {
	S3Endpoint string
	S3ID       string
	S3Key      string
	S3Bucket   string
	S3Region   string
	S3Prefix   string
	S3Secure   bool
	// S3SessionToken is only needed with temporary credentials (AWS STS).
	S3SessionToken string
	// S3URLStyle is "auto", "path" or "virtual".
	S3URLStyle string

	// PartSize is the size of the multipart upload parts. As the archive is
	// streamed, one part is buffered in memory and the archive can be at most
	// 10,000 parts.
	PartSize uint64

	// RetentionDays is the number of days backups are kept; 0 disables pruning.
	RetentionDays int
	// KeepMin is the number of most recent backups never deleted, whatever
	// their age.
	KeepMin       int
	PresignExpiry time.Duration

	MongoURI      string
	MongodumpPath string
	Gzip          bool
	Oplog         bool

	// Recipients are the age public keys the archive is encrypted for;
	// encryption is disabled when empty.
	Recipients []age.Recipient

	// SizeAlertPercent raises a warning (and skips retention) when the backup
	// is that much smaller than the previous one; 0 disables the check.
	SizeAlertPercent int

	// Name identifies this machine in notifications (hostname by default).
	Name       string
	DiscordURL string
}

// loadConfig reads the configuration from the environment and validates it.
// getenv is injected to make testing easy (os.Getenv in production).
func loadConfig(getenv func(string) string) (*config, error) {
	cfg := &config{
		S3Endpoint:     strings.TrimSpace(getenv("S3_ENDPOINT")),
		S3ID:           getenv("S3_ID"),
		S3Key:          getenv("S3_KEY"),
		S3Bucket:       strings.TrimSpace(getenv("S3_BUCKET")),
		S3Region:       strings.TrimSpace(getenv("S3_REGION")),
		S3Prefix:       strings.TrimLeft(strings.TrimSpace(getenv("S3_PREFIX")), "/"),
		S3URLStyle:     strings.ToLower(strings.TrimSpace(getenv("S3_URL_STYLE"))),
		S3SessionToken: getenv("S3_SESSION_TOKEN"),
		MongoURI:       strings.TrimSpace(getenv("MONGO_URI")),
		MongodumpPath:  strings.TrimSpace(getenv("MONGODUMP_PATH")),
		Name:           strings.TrimSpace(getenv("BACKUP_NAME")),
		DiscordURL:     strings.TrimSpace(getenv("DISCORD_URL")),
	}

	var errs []error

	for _, required := range []struct{ name, value string }{
		{"S3_BUCKET", cfg.S3Bucket},
		{"MONGO_URI", cfg.MongoURI},
	} {
		if required.value == "" {
			errs = append(errs, fmt.Errorf("%s is required", required.name))
		}
	}

	// Without static credentials, the standard AWS chain is used instead
	// (AWS_* variables, ~/.aws/credentials, IAM role...).
	if (cfg.S3ID == "") != (cfg.S3Key == "") {
		errs = append(errs, errors.New("S3_ID and S3_KEY must be set together"))
	}

	if cfg.S3Endpoint == "" {
		cfg.S3Endpoint = "s3.amazonaws.com"
	}
	// Tolerate an endpoint written with its scheme, which minio-go rejects.
	if rest, ok := strings.CutPrefix(cfg.S3Endpoint, "https://"); ok {
		cfg.S3Endpoint = rest
	} else if rest, ok := strings.CutPrefix(cfg.S3Endpoint, "http://"); ok {
		cfg.S3Endpoint = rest
	}
	cfg.S3Endpoint = strings.TrimRight(cfg.S3Endpoint, "/")

	// Cloudflare R2 does not use real regions, "auto" avoids a location lookup.
	if cfg.S3Region == "" && strings.HasSuffix(cfg.S3Endpoint, ".r2.cloudflarestorage.com") {
		cfg.S3Region = "auto"
	}

	switch cfg.S3URLStyle {
	case "":
		cfg.S3URLStyle = "auto"
	case "auto", "path", "virtual":
	default:
		errs = append(errs, fmt.Errorf("S3_URL_STYLE must be auto, path or virtual, got %q", cfg.S3URLStyle))
	}

	if cfg.MongodumpPath == "" {
		cfg.MongodumpPath = "mongodump"
	}

	var err error
	if cfg.S3Secure, err = parseBool(getenv("S3_SECURE"), true); err != nil {
		errs = append(errs, fmt.Errorf("S3_SECURE: %w", err))
	}
	if cfg.Gzip, err = parseBool(getenv("MONGODUMP_GZIP"), true); err != nil {
		errs = append(errs, fmt.Errorf("MONGODUMP_GZIP: %w", err))
	}

	if cfg.Oplog, err = parseBool(getenv("MONGODUMP_OPLOG"), false); err != nil {
		errs = append(errs, fmt.Errorf("MONGODUMP_OPLOG: %w", err))
	}

	if cfg.RetentionDays, err = parseInt(getenv("S3_RETENTION"), 0, 0, 1<<20); err != nil {
		errs = append(errs, fmt.Errorf("S3_RETENTION must be a positive number of days: %w", err))
	}
	if cfg.KeepMin, err = parseInt(getenv("S3_KEEP_MIN"), 3, 1, 1<<20); err != nil {
		errs = append(errs, fmt.Errorf("S3_KEEP_MIN must be at least 1: %w", err))
	}
	if cfg.SizeAlertPercent, err = parseInt(getenv("SIZE_ALERT_PERCENT"), 30, 0, 100); err != nil {
		errs = append(errs, fmt.Errorf("SIZE_ALERT_PERCENT must be a percentage: %w", err))
	}
	partSize, err := parseInt(getenv("S3_PART_SIZE"), 64, minPartSizeMiB, maxPartSizeMiB)
	if err != nil {
		errs = append(errs, fmt.Errorf("S3_PART_SIZE must be between %d and %d MiB: %w", minPartSizeMiB, maxPartSizeMiB, err))
	}
	cfg.PartSize = uint64(partSize) << 20

	if cfg.Recipients, err = parseRecipients(getenv("AGE_RECIPIENTS"), strings.TrimSpace(getenv("AGE_RECIPIENTS_FILE"))); err != nil {
		errs = append(errs, err)
	}

	cfg.PresignExpiry = maxPresignExpiry
	if raw := strings.TrimSpace(getenv("PRESIGN_EXPIRY")); raw != "" {
		cfg.PresignExpiry, err = time.ParseDuration(raw)
		if err != nil || cfg.PresignExpiry < time.Second || cfg.PresignExpiry > maxPresignExpiry {
			errs = append(errs, fmt.Errorf("PRESIGN_EXPIRY must be a duration between 1s and 168h, got %q", raw))
		}
	}

	return cfg, errors.Join(errs...)
}

// parseRecipients reads age recipients (age1..., age1pq1... or ssh-ed25519 /
// ssh-rsa public keys) separated by commas or newlines, and from an optional
// recipients file in the same format as `age -R`.
func parseRecipients(inline, file string) ([]age.Recipient, error) {
	lines := strings.FieldsFunc(inline, func(r rune) bool { return r == ',' || r == '\n' })
	if file != "" {
		content, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("AGE_RECIPIENTS_FILE: %w", err)
		}
		lines = append(lines, strings.Split(string(content), "\n")...)
	}

	var recipients []age.Recipient
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		var (
			recipient age.Recipient
			err       error
		)
		if strings.HasPrefix(line, "ssh-") {
			recipient, err = agessh.ParseRecipient(line)
		} else {
			var parsed []age.Recipient
			parsed, err = age.ParseRecipients(strings.NewReader(line))
			if err == nil {
				recipient = parsed[0]
			}
		}
		if err != nil {
			return nil, fmt.Errorf("invalid age recipient %q: %w", line, err)
		}
		recipients = append(recipients, recipient)
	}
	return recipients, nil
}

func parseInt(raw string, def, min, max int) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def, err
	}
	if n < min || n > max {
		return def, fmt.Errorf("%d is out of range", n)
	}
	return n, nil
}

func parseBool(raw string, def bool) (bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def, nil
	}
	return strconv.ParseBool(raw)
}
