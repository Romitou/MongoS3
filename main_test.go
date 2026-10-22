package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/minio/minio-go/v7"
)

func envFrom(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func validEnv() map[string]string {
	return map[string]string{
		"S3_ENDPOINT": "https://s3.example.com/",
		"S3_ID":       "id",
		"S3_KEY":      "key",
		"S3_BUCKET":   "backups",
		"MONGO_URI":   "mongodb://user:secret@localhost:27017",
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(envFrom(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.S3Endpoint != "s3.example.com" {
		t.Errorf("endpoint scheme should be stripped, got %q", cfg.S3Endpoint)
	}
	if !cfg.S3Secure || !cfg.Gzip {
		t.Error("S3_SECURE and MONGODUMP_GZIP should default to true")
	}
	if cfg.RetentionDays != 0 {
		t.Errorf("retention should default to 0, got %d", cfg.RetentionDays)
	}
	if cfg.PresignExpiry != maxPresignExpiry {
		t.Errorf("presign expiry should default to 7 days, got %s", cfg.PresignExpiry)
	}
	if cfg.S3URLStyle != "auto" || cfg.S3Region != "" {
		t.Errorf("unexpected URL style %q or region %q", cfg.S3URLStyle, cfg.S3Region)
	}
	if cfg.MongodumpPath != "mongodump" {
		t.Errorf("unexpected mongodump path %q", cfg.MongodumpPath)
	}
}

func TestLoadConfigErrors(t *testing.T) {
	tests := map[string]struct {
		key, value, want string
	}{
		"missing bucket":     {"S3_BUCKET", "", "S3_BUCKET is required"},
		"missing mongo uri":  {"MONGO_URI", " ", "MONGO_URI is required"},
		"negative retention": {"S3_RETENTION", "-1", "S3_RETENTION"},
		"invalid retention":  {"S3_RETENTION", "ten", "S3_RETENTION"},
		"invalid secure":     {"S3_SECURE", "maybe", "S3_SECURE"},
		"key without id":     {"S3_ID", "", "S3_ID and S3_KEY"},
		"invalid url style":  {"S3_URL_STYLE", "dns", "S3_URL_STYLE"},
		"expiry too long":    {"PRESIGN_EXPIRY", "200h", "PRESIGN_EXPIRY"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			env := validEnv()
			env[tt.key] = tt.value
			_, err := loadConfig(envFrom(env))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected error containing %q, got %v", tt.want, err)
			}
		})
	}
}

func TestLoadConfigProviders(t *testing.T) {
	env := validEnv()
	env["S3_ENDPOINT"] = "https://0123abcd.r2.cloudflarestorage.com"
	cfg, err := loadConfig(envFrom(env))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.S3Region != "auto" {
		t.Errorf("R2 region should default to auto, got %q", cfg.S3Region)
	}

	// AWS with credentials from the environment chain (IAM role, profile...).
	env = validEnv()
	delete(env, "S3_ENDPOINT")
	delete(env, "S3_ID")
	delete(env, "S3_KEY")
	cfg, err = loadConfig(envFrom(env))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.S3Endpoint != "s3.amazonaws.com" {
		t.Errorf("endpoint should default to AWS, got %q", cfg.S3Endpoint)
	}
}

func TestUriPassword(t *testing.T) {
	tests := map[string]string{
		"mongodb://user:secret@localhost:27017":                  "secret",
		"mongodb+srv://user:p%40ss@cluster.example.net/db?ssl=1": "p%40ss",
		"mongodb://u:pw@h1:27017,h2:27017/?replicaSet=rs0":       "pw",
		"mongodb://localhost:27017":                              "",
		"mongodb://localhost/?authSource=a@b":                    "",
		"not a uri":                                              "",
	}
	for uri, want := range tests {
		if got := uriPassword(uri); got != want {
			t.Errorf("uriPassword(%q) = %q, want %q", uri, got, want)
		}
	}
}

func TestRedactSecret(t *testing.T) {
	uri := "mongodb://user:p%40ss@localhost"
	got := redactSecret("connecting with p%40ss and p@ss", uri)
	if want := "connecting with **** and ****"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestHumanSize(t *testing.T) {
	tests := map[int64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KiB", 5 << 30: "5.0 GiB"}
	for in, want := range tests {
		if got := humanSize(in); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestTruncateHead(t *testing.T) {
	if got := truncateHead("short", 10); got != "short" {
		t.Errorf("got %q", got)
	}
	got := truncateHead(strings.Repeat("a", 50)+"the error", 20)
	if len([]rune(got)) != 20 || !strings.HasSuffix(got, "the error") {
		t.Errorf("got %q", got)
	}
}

func TestFailureMessageFitsDiscordLimits(t *testing.T) {
	msg := failureMessage(failureReport{Details: strings.Repeat("é", 10000) + "```"}, time.Now())
	desc := msg.Embeds[0].Description
	if n := len([]rune(desc)); n > maxDescriptionLength {
		t.Errorf("description is %d characters long", n)
	}
	if strings.Count(desc, "```") != 2 {
		t.Error("details must not break the code block")
	}
}

func TestSuccessMessageOmitsMissingLinks(t *testing.T) {
	msg := successMessage(backupReport{Duration: 90 * time.Second, ArchiveSize: 2048, RetentionDays: 7, Pruned: 2}, time.Now())
	for _, f := range msg.Embeds[0].Fields {
		if strings.Contains(f.Value, "](") {
			t.Errorf("unexpected link field %q", f.Name)
		}
	}
	if v := msg.Embeds[0].Fields[0].Value; v != "1m30s" {
		t.Errorf("unexpected duration %q", v)
	}
}

func TestDiscordSend(t *testing.T) {
	var received discordMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("unexpected content type %q", ct)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &received); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	if err := newDiscordNotifier(server.URL).notifyFailure(context.Background(), failureReport{Details: "boom"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(received.Embeds) != 1 || !strings.Contains(received.Embeds[0].Description, "boom") {
		t.Errorf("unexpected payload %+v", received)
	}
}

func TestDiscordSendReportsHTTPErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "invalid webhook", http.StatusBadRequest)
	}))
	defer server.Close()

	err := newDiscordNotifier(server.URL).notifyFailure(context.Background(), failureReport{Details: "boom"}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("expected HTTP error, got %v", err)
	}
}

func TestLoadConfigRecipients(t *testing.T) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	env := validEnv()
	env["AGE_RECIPIENTS"] = identity.Recipient().String() + ", ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl comment"
	cfg, err := loadConfig(envFrom(env))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Recipients) != 2 {
		t.Errorf("expected 2 recipients, got %d", len(cfg.Recipients))
	}

	env["AGE_RECIPIENTS"] = "AGE-SECRET-KEY-1..."
	if _, err := loadConfig(envFrom(env)); err == nil {
		t.Error("an invalid recipient should be rejected")
	}
}

func TestParseDumpStats(t *testing.T) {
	logs := `2026-01-01T00:00:00.000+0000	writing shop.orders to archive on stdout
2026-01-01T00:00:00.000+0000	done dumping shop.orders (1234 documents)
2026-01-01T00:00:00.000+0000	done dumping shop.users (1 document)
2026-01-01T00:00:00.000+0000	done dumping admin.system.version (2 documents)`
	got := parseDumpStats(logs)
	want := dumpStats{Databases: 2, Collections: 3, Documents: 1237}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestBackupBase(t *testing.T) {
	tests := []struct {
		key, base string
		archive   bool
	}{
		{"dump.2026-01-01T000000Z.archive.gz.age", "dump.2026-01-01T000000Z", true},
		{"dump.2026-01-01T000000Z.log", "dump.2026-01-01T000000Z", false},
		{"dump.2022-01-01T150405.logs", "dump.2022-01-01T150405", false},
		{"my.logs/dump.2026-01-01T000000Z.archive", "my.logs/dump.2026-01-01T000000Z", true},
	}
	for _, tt := range tests {
		base, archive := backupBase(tt.key)
		if base != tt.base || archive != tt.archive {
			t.Errorf("backupBase(%q) = %q, %v", tt.key, base, archive)
		}
	}
}

func object(key string, daysAgo int, size int64) minio.ObjectInfo {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	return minio.ObjectInfo{Key: key, LastModified: now.AddDate(0, 0, -daysAgo), Size: size}
}

func TestRetention(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	sets := groupBackups([]minio.ObjectInfo{
		object("dump.d40.archive.gz", 40, 100),
		object("dump.d40.log", 40, 1),
		object("dump.d30.archive.gz", 30, 100),
		object("dump.d20.log", 20, 1), // failed backup: logs without archive
		object("dump.d10.archive.gz", 10, 100),
		object("dump.d0.archive.gz", 0, 50),
	})
	if len(sets) != 5 || sets[0].Base != "dump.d0" {
		t.Fatalf("unexpected grouping: %+v", sets)
	}

	names := func(sets []*backupSet) string {
		var out []string
		for _, s := range sets {
			out = append(out, s.Base)
		}
		return strings.Join(out, ",")
	}

	// 7 days retention, keep at least 2 archives: d0 and d10 are kept.
	if got := names(expiredBackups(sets, now.AddDate(0, 0, -7), 2)); got != "dump.d20,dump.d30,dump.d40" {
		t.Errorf("unexpected expired backups %q", got)
	}
	// Keep at least 3 archives: d30 is kept although it is old.
	if got := names(expiredBackups(sets, now.AddDate(0, 0, -7), 3)); got != "dump.d20,dump.d40" {
		t.Errorf("unexpected expired backups %q", got)
	}
	if got := len(expiredBackups(sets[1:2], now.AddDate(0, 0, -7), 1)); got != 0 {
		t.Errorf("the last archive must never be deleted, got %d expired", got)
	}

	previous := previousArchive(sets, "dump.d0", ".archive.gz")
	if previous == nil || previous.Key != "dump.d10.archive.gz" {
		t.Fatalf("unexpected previous archive %+v", previous)
	}
	if previousArchive(sets, "dump.d0", ".archive.gz.age") != nil {
		t.Error("archives with another extension must not be compared")
	}
	if drop := sizeDrop(previous.Size, 50); drop != 50 {
		t.Errorf("unexpected size drop %v", drop)
	}
}

func TestFormatCount(t *testing.T) {
	tests := map[int64]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567", -1234: "-1,234"}
	for in, want := range tests {
		if got := formatCount(in); got != want {
			t.Errorf("formatCount(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestSuccessMessageSizeAlert(t *testing.T) {
	msg := successMessage(backupReport{ArchiveSize: 40, PreviousSize: 100, SizeAlert: true, RetentionDays: 7, RetentionSkipped: true, Name: "db-1"}, time.Now())
	embed := msg.Embeds[0]
	if embed.Color != colorWarning || !strings.Contains(embed.Description, "60% smaller") {
		t.Errorf("unexpected embed %+v", embed)
	}
	if embed.Footer == nil || !strings.Contains(embed.Footer.Text, "db-1") {
		t.Error("the machine name should be in the footer")
	}
}

func TestFailureMessageSteps(t *testing.T) {
	msg := failureMessage(failureReport{Step: "Dump & upload", Details: "boom", Steps: []step{{"S3 check", time.Second}, {"Dump & upload", time.Minute}}}, time.Now())
	fields := msg.Embeds[0].Fields
	if len(fields) != 2 || fields[0].Value != "Dump & upload" || !strings.Contains(fields[1].Value, "❌ Dump & upload: 1m0s") {
		t.Errorf("unexpected fields %+v", fields)
	}
}
