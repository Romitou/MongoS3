# Mongo S3 🍃
A simple utility for backing up MongoDB databases to an S3 server. Backing up a binary archive of your database to an S3 server ensures that your data is not lost, whatever happens to your server or its replication.

## ✨ Features
* Dumps the whole cluster with `mongodump` into a single gzipped archive, **streamed** to any S3-compatible server (AWS, Cloudflare R2, MinIO, Scaleway, OVH, Backblaze...): nothing is written to disk, whatever the size of the database
* Encrypts the archive with [age](https://age-encryption.org) public keys (or SSH keys) before it leaves the server
* Point-in-time consistent backups of replica sets with `--oplog`
* Deletes the backups older than the retention period, while always keeping the last ones
* Warns when a backup is much smaller than the previous one, and then keeps old backups untouched
* Sends a Discord notification with the size, content (databases, collections, documents), duration of each step and download links, or the failed step and error details on failure
* Keeps the MongoDB password out of the process list and out of the logs
* Exits with a non-zero status on failure, so it plays well with cron, systemd timers or Kubernetes CronJobs

## 🚀 Installation
First of all, this application uses the `mongodump` command, which is part of the [`mongodb-database-tools`](https://www.mongodb.com/docs/database-tools/installation/) package (version 100.3.0 or newer). Refer to the documentation of your operating system to install this package.

Secondly, download the binary! Binaries are released for `Linux AMD64`, `Linux ARM64`, `Linux ARM`, `Windows AMD64` and `macOS ARM64`. Feel free to open an issue if your architecture is not listed. For example, if your distribution is Linux and your architecture is `AMD64`:
```
curl -L https://github.com/Romitou/MongoS3/releases/latest/download/mongobackup-linux-amd64 -o mongobackup
chmod +x mongobackup
```

You can also build it yourself with Go: `go build -o mongobackup .`

## ⚙️ Configuration
The configuration is read from the environment. For convenience, a `.env` file in the working directory is loaded if it exists (see [`.env.example`](.env.example)).

| Variable | Required | Default | Description |
|---|---|---|---|
| `S3_ENDPOINT` | | `s3.amazonaws.com` | Endpoint of your S3 server (see [providers](#-s3-providers)) |
| `S3_ID` | | | Access key ID, see below when empty |
| `S3_KEY` | | | Secret access key |
| `S3_SESSION_TOKEN` | | | Session token, only for temporary credentials (AWS STS) |
| `S3_BUCKET` | ✅ | | Bucket where the backups are stored (it must exist) |
| `S3_REGION` | | auto | Region of the bucket |
| `S3_PREFIX` | | | Prefix of the object names, e.g. `mongodb/` to store them in a folder |
| `S3_SECURE` | | `true` | Set to `false` to use plain HTTP (local MinIO for instance) |
| `S3_URL_STYLE` | | `auto` | `path` (`endpoint/bucket/key`) or `virtual` (`bucket.endpoint/key`), for providers supporting only one of them |
| `S3_RETENTION` | | `0` | Number of days backups are retained, older ones are deleted. `0` keeps them forever |
| `S3_KEEP_MIN` | | `3` | Number of most recent backups never deleted, whatever their age |
| `S3_PART_SIZE` | | `64` | Size in MiB of the upload parts. One part is kept in memory, and an archive is at most 10,000 parts (640 GiB by default) |
| `PRESIGN_EXPIRY` | | `168h` | Validity of the download links sent to Discord (7 days at most) |
| `MONGO_URI` | ✅ | | The MongoDB [connection string](https://www.mongodb.com/docs/manual/reference/connection-string/) of your cluster |
| `MONGODUMP_PATH` | | `mongodump` | Path to the `mongodump` binary |
| `MONGODUMP_GZIP` | | `true` | Compress the archive |
| `MONGODUMP_OPLOG` | | `false` | Point-in-time snapshot with `--oplog`, see [consistency](#-consistency) |
| `AGE_RECIPIENTS` | | | Public keys to encrypt the archive for, comma separated, see [encryption](#-encryption) |
| `AGE_RECIPIENTS_FILE` | | | File with one public key per line, same format as `age -R` |
| `SIZE_ALERT_PERCENT` | | `30` | Warn when the archive is that much smaller than the previous one. `0` disables it |
| `BACKUP_NAME` | | hostname | Name of this server in notifications |
| `DISCORD_URL` | | | Discord webhook for notifications, disabled when empty |

When `S3_ID` and `S3_KEY` are empty, credentials are looked up like the AWS CLI does: `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` variables, `~/.aws/credentials` (`AWS_PROFILE`), then the IAM role of the EC2 instance / ECS task.

Retention only deletes objects whose name starts with `S3_PREFIX` + `dump.`, other objects of the bucket are never touched. The `S3_KEEP_MIN` most recent backups are always kept: if backups keep failing for longer than the retention period, the last good ones are not deleted. Retention is also skipped when the size alert is raised, so a suspicious backup never causes old ones to be deleted.

## 🔒 Encryption
The archive can be encrypted with [age](https://age-encryption.org) before being uploaded: neither the S3 provider nor anyone getting the download link can read it. Only public keys are needed on the server, the private key can stay offline.

Generate a key pair (on your computer, not on the server), and keep `key.txt` safe: without it, backups cannot be restored.
```
age-keygen -o key.txt
# Public key: age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p
```
Then set `AGE_RECIPIENTS=age1ql3z7hjy...`. Several keys can be given, separated by commas, any of them can decrypt. SSH public keys (`ssh-ed25519 ...`, `ssh-rsa ...`) work too, decrypt with `age -d -i ~/.ssh/id_ed25519`.

The logs are not encrypted, they only contain the names of the databases and collections.

## 🧾 Consistency
Without `--oplog`, writes happening during the dump can leave the backup in an inconsistent state (a collection dumped before a change, another one after). On a replica set, set `MONGODUMP_OPLOG=true`: the oplog entries written during the dump are included and replayed on restore, which gives a snapshot of the moment the dump ended. It is not supported on standalone servers or through `mongos`, and the user needs to read `local.oplog.rs` (the `backup` role is enough).

## ☁️ S3 providers
Any S3-compatible server works. Examples of configuration:

| Provider | `S3_ENDPOINT` | `S3_REGION` | Notes |
|---|---|---|---|
| AWS S3 | `s3.amazonaws.com` (default) | your bucket region, e.g. `eu-west-3` | Credentials can come from an IAM role |
| Cloudflare R2 | `<account-id>.r2.cloudflarestorage.com` | `auto` (default for R2) | Use an R2 API token's access key ID and secret |
| MinIO | `minio.example.com:9000` | | `S3_SECURE=false` without TLS |
| Scaleway | `s3.fr-par.scw.cloud` | `fr-par` | |
| OVHcloud | `s3.gra.io.cloud.ovh.net` | `gra` | |
| Backblaze B2 | `s3.us-west-004.backblazeb2.com` | `us-west-004` | |
| Google Cloud Storage | `storage.googleapis.com` | | Needs [HMAC keys](https://cloud.google.com/storage/docs/authentication/hmackeys) |

The access key needs the permissions to list, read, write and delete objects in the bucket (`s3:ListBucket`, `s3:GetObject`, `s3:PutObject`, `s3:DeleteObject` on AWS). Download links of the Discord notification are presigned URLs: they work even if the bucket is private.

## ⏰ Scheduling
Run the application periodically to backup your database, for example every day at midnight with crontab:
```
0 0 * * * cd /opt/mongobackup && ./mongobackup >> backup.log 2>&1
```

## ♻️ Restoring a backup
Download the archive (the Discord notification contains a link), then:
```
mongorestore --uri="mongodb://..." --gzip --archive=dump.2026-01-01T000000Z.archive.gz
```
For an encrypted archive, decrypt it on the fly:
```
age -d -i key.txt dump.2026-01-01T000000Z.archive.gz.age | mongorestore --uri="mongodb://..." --gzip --archive
```
Omit `--gzip` for backups made with `MONGODUMP_GZIP=false`, and add `--oplogReplay` for backups made with `MONGODUMP_OPLOG=true`.

## 🖼️ Screenshots
![](https://github.com/Romitou/MongoS3/raw/main/screenshots/success.jpg)
![](https://github.com/Romitou/MongoS3/raw/main/screenshots/error.jpg)
