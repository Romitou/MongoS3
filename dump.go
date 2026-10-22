package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// mongoDump dumps the whole cluster as an archive written to out and returns
// the mongodump logs (with the MongoDB password redacted).
func mongoDump(ctx context.Context, cfg *config, workDir string, out io.Writer) (string, error) {
	// The URI is passed through a config file readable only by us, so the
	// credentials never show up in the process list (ps, /proc/<pid>/cmdline).
	configPath := filepath.Join(workDir, "mongodump.yaml")
	if err := os.WriteFile(configPath, []byte("uri: "+strconv.Quote(cfg.MongoURI)+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write mongodump config: %w", err)
	}
	defer os.Remove(configPath)

	// Without a value, --archive writes to stdout; logs go to stderr.
	args := []string{"-vvv", "--config=" + configPath, "--archive"}
	if cfg.Gzip {
		args = append(args, "--gzip")
	}
	if cfg.Oplog {
		args = append(args, "--oplog")
	}

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, cfg.MongodumpPath, args...)
	cmd.Stdout = out
	cmd.Stderr = &stderr
	err := cmd.Run()
	logs := redactSecret(stderr.String(), cfg.MongoURI)
	if err != nil {
		return logs, fmt.Errorf("mongodump: %w", err)
	}
	return logs, nil
}

// dumpStats sums up what mongodump has dumped.
type dumpStats struct {
	Databases   int
	Collections int
	Documents   int64
}

// doneDumpingPattern matches mongodump lines such as
// "2026-01-01T00:00:00.000+0000	done dumping shop.orders (1234 documents)".
var doneDumpingPattern = regexp.MustCompile(`done dumping (\S+) \((\d+) documents?\)`)

func parseDumpStats(logs string) dumpStats {
	var stats dumpStats
	databases := map[string]bool{}
	for _, match := range doneDumpingPattern.FindAllStringSubmatch(logs, -1) {
		database, _, _ := strings.Cut(match[1], ".")
		databases[database] = true
		stats.Collections++
		count, _ := strconv.ParseInt(match[2], 10, 64)
		stats.Documents += count
	}
	stats.Databases = len(databases)
	return stats
}

// redactSecret hides the password of a MongoDB connection string if it
// appears in text.
func redactSecret(text, uri string) string {
	password := uriPassword(uri)
	if password == "" {
		return text
	}
	text = strings.ReplaceAll(text, password, "****")
	if decoded, err := url.PathUnescape(password); err == nil && decoded != password {
		text = strings.ReplaceAll(text, decoded, "****")
	}
	return text
}

// uriPassword extracts the raw (still percent-encoded) password of a
// mongodb:// or mongodb+srv:// URI. url.Parse cannot be used because of
// multi-host URIs such as mongodb://u:p@h1:27017,h2:27017/.
func uriPassword(uri string) string {
	_, rest, ok := strings.Cut(uri, "://")
	if !ok {
		return ""
	}
	if i := strings.IndexAny(rest, "/?"); i >= 0 {
		rest = rest[:i]
	}
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return ""
	}
	_, password, _ := strings.Cut(rest[:at], ":")
	return password
}
