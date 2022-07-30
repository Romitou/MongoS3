package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/joho/godotenv"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"
)

var startTime = time.Now()
var backupName = "dump." + startTime.Format("2006-01-02T150405") + ".archive"
var logsName = "dump." + startTime.Format("2006-01-02T150405") + ".logs"

var backupPath = path.Join("temp", backupName)

func main() {
	startTime = time.Now()
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Error loading .env file")
	}

	minioClient, err := minio.New(os.Getenv("S3_ENDPOINT"), &minio.Options{
		Creds:  credentials.NewStaticV4(os.Getenv("S3_ID"), os.Getenv("S3_KEY"), ""),
		Secure: true,
	})
	if err != nil {
		log.Fatal(err)
	}

	log.Println("Dumping Mongo database...")
	dumpLogs := mongoDump()

	log.Println("Sending archive to S3 server...")
	archiveInfo, logsInfo := sendToS3(minioClient, dumpLogs)

	log.Println("Cleaning temp archive...")
	cleanDump()

	log.Println("Retrieving a presigned URL for the backup object from S3 server...")
	presignedArchive, presignedLogs := getPresignedURL(minioClient, archiveInfo, logsInfo)

	log.Println("Send a notification to Discord...")
	sendDiscordSuccess(presignedArchive.String(), presignedLogs.String(), archiveInfo)
}

func mongoDump() string {
	output, err := exec.Command("mongodump", "-vvv --uri=\""+os.Getenv("MONGO_URI")+"\"", "--archive="+backupPath).CombinedOutput()
	if err != nil {
		sendDiscordError(string(output) + "\n\n" + err.Error())
		log.Fatal(err)
	}
	return string(output)
}

func sendToS3(client *minio.Client, dumpLogs string) (archiveInfo minio.UploadInfo, logsInfo minio.UploadInfo) {
	archiveInfo, err := client.FPutObject(context.Background(), os.Getenv("S3_BUCKET"), backupName, backupPath, minio.PutObjectOptions{ContentType: "application/octet-stream"})
	if err != nil {
		marshaledInfo, _ := json.Marshal(archiveInfo)
		sendDiscordError(string(marshaledInfo) + "\n\n" + err.Error())
		log.Fatal(err)
	}

	reader := strings.NewReader(dumpLogs)
	logsInfo, err = client.PutObject(context.Background(), os.Getenv("S3_BUCKET"), logsName, reader, reader.Size(), minio.PutObjectOptions{ContentType: "text/plain"})
	if err != nil {
		log.Println(err)
	}

	return
}

func cleanDump() {
	err := os.Remove(backupPath)
	if err != nil {
		sendDiscordError(err.Error())
		log.Fatal(err)
	}
}

func getPresignedURL(client *minio.Client, archiveInfo minio.UploadInfo, logsInfo minio.UploadInfo) (presignedArchive *url.URL, presignedLogs *url.URL) {
	presignedArchive, err := client.PresignedGetObject(context.Background(), archiveInfo.Bucket, archiveInfo.Key, time.Hour*7*24, nil)
	if err != nil {
		sendDiscordError(err.Error())
		log.Println(err)
	}

	presignedLogs, err = client.PresignedGetObject(context.Background(), logsInfo.Bucket, logsInfo.Key, time.Hour*7*24, nil)
	if err != nil {
		log.Println(err)
	}

	return
}

func sendDiscordSuccess(presignedArchive string, presignedLogs string, archiveInfo minio.UploadInfo) {
	sendDiscordMessage("{\"content\":null,\"embeds\":[{\"title\":\"New MongoDB backup\",\"description\":\"A new backup of the database has been performed today, at " + time.Now().Format("15:04:05") + ". This backup has been sent to the S3 backup server.\",\"color\":5832563,\"fields\":[{\"name\":\"\\u231b Execution time\",\"value\":\"" + strconv.Itoa(int(time.Now().Unix()-startTime.Unix())) + "ms\",\"inline\":true},{\"name\":\"\\u2705 Retention period\",\"value\":\"" + os.Getenv("S3_RETENTION") + " days\",\"inline\":true},{\"name\":\"\\ud83d\\udcc4 Backup size\",\"value\":\"" + strconv.Itoa(int(archiveInfo.Size)/(1<<10)) + " KB\",\"inline\":true},{\"name\":\"\\ud83d\\udcdc Backup logs\",\"value\":\"[Click to open the generated logs](" + presignedLogs + ")\",\"inline\":true},{\"name\":\"\\ud83d\\udce5 Download link\",\"value\":\"[Click to download the backup archive](" + presignedArchive + ")\",\"inline\":true}]}],\"attachments\":[]}")
}

func sendDiscordError(info string) {

	jsonInfo := jsonEncode(info)

	sendDiscordMessage("{\"content\":null,\"embeds\":[{\"title\":\"Failed MongoDB backup\",\"description\":\"A backup attempt of the MongoDB database has been performed today at " + time.Now().Format("15:04:05") + ". An error was encountered during the backup process, you can find all the information below.\",\"color\":16734296,\"fields\":[{\"name\":\"\\ud83d\\udd0e Backup error\",\"value\":\"```" + string(jsonInfo) + "```\"}]}],\"attachments\":[]}")
}

func sendDiscordMessage(json string) {
	_, err := http.Post(os.Getenv("DISCORD_URL"), "application/json", bytes.NewBufferString(json))
	if err != nil {
		log.Println(err)
		return
	}
}

func jsonEncode(jsonString string) string {
	b, err := json.Marshal(jsonString)
	if err != nil {
		return ""
	}
	return string(b[1 : len(b)-1])
}
