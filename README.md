# Mongo S3 🍃
A simple utility for backing up MongoDB databases to an S3 server. The utility of backing up a binary archive of your database to an S3 server is the security that your data is not lost through, depending on your server, replication.

## 🚀 Installation
First of all, this application uses the `mongodump` command, which is part of the `mongodb-database-tools` package. Refer to the documentation of your operating system to install this package.

Secondly, download the binary! Currently, three types of binaries have been released: `Linux AMD64`, `Linux ARM` and `Windows AMD64`. Feel free to open an issue if your architecture is not listed. Having said that, all you have to do is download the latest binary, for example if your distribution is Linux and your architecture is `AMD64` :
```
curl -L https://github.com/Romitou/MongoS3/releases/latest/download/mongobackup-linux-amd64 -o mongobackup
```

Thirdly, make sure that the process is able to write files, especially in the `temp` folder, relative to where the process is located.

Next, write the environment file `.env`. Here are the values you need to specify:
* `S3_ENDPOINT` : endpoint of your S3 server (do not include HTTP scheme)
* `S3_ID` : username or identifier to access the S3 server
* `S3_KEY` : key or password to authenticate to the S3 server
* `S3_BUCKET` : the bucket where the backups should be stored
* `S3_RETENTION` : the number of days your backups are retained
* `DISCORD_URL` : the address of the Discord backup notification webhook
* `MONGO_URI` : the MongoDB address ([see documentation](https://www.mongodb.com/docs/manual/reference/connection-string/)) to connect to your cluster

Finally, you can run this application periodically (e.g. every day at midnight) to backup your database. This can be done using crontab, depending on your operating system.

## 🖼️ Screenshots
![](https://github.com/Romitou/MongoS3/raw/main/screenshots/success.jpg)
![](https://github.com/Romitou/MongoS3/raw/main/screenshots/error.jpg)