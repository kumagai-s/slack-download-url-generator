package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

var api = slack.New("TOKEN")

func main() {
	signingSecret := os.Getenv("SLACK_SIGHNG_SECRET")

	http.HandleFunc("/uploader-v2/upload", func(w http.ResponseWriter, r *http.Request) {
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		sv, err := slack.NewSecretsVerifier(r.Header, signingSecret)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if _, err := sv.Write(body); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if err := sv.Ensure(); err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		eventsAPIEvent, err := slackevents.ParseEvent(json.RawMessage(body), slackevents.OptionNoVerifyToken())
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		if eventsAPIEvent.Type == slackevents.URLVerification {
			var r *slackevents.ChallengeResponse
			err := json.Unmarshal([]byte(body), &r)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text")
			w.Write([]byte(r.Challenge))
		}
		if eventsAPIEvent.Type == slackevents.CallbackEvent {
			innerEvent := eventsAPIEvent.InnerEvent
			switch ev := innerEvent.Data.(type) {
			case *slackevents.AppMentionEvent:
				timestamp, _ := strconv.ParseInt(ev.TimeStamp, 10, 64)
				files, _, err := api.GetFiles(slack.GetFilesParameters{
					User:          ev.User,
					Channel:       ev.Channel,
					TimestampFrom: slack.JSONTime(timestamp),
				})
				if err != nil {
					log.Fatal(err)
					return
				}

				for _, file := range files {
					url := file.URLPrivateDownload

					res, err := http.Get(url)
					if err != nil {
						panic(err)
					}
					defer res.Body.Close()

					customResolver := aws.EndpointResolverFunc(func(service, region string) (aws.Endpoint, error) {
						if os.Getenv("APP_ENV") == "dev" {
							return aws.Endpoint{
								PartitionID:   "aws",
								URL:           "http://minio:9000",
								SigningRegion: "ap-northeast-1",
							}, nil
						}
						return aws.Endpoint{}, &aws.EndpointNotFoundError{}
					})
					sdkconfig, err := config.LoadDefaultConfig(context.TODO(), config.WithEndpointResolver(customResolver))
					s3Client := s3.NewFromConfig(sdkconfig, func(o *s3.Options) {
						o.Credentials = aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider("root", "password", ""))
						o.UsePathStyle = true
					})
					file, err := ioutil.ReadAll(res.Body)
					_, err = s3Client.PutObject(context.TODO(), &s3.PutObjectInput{
						Bucket:      aws.String("uploader-v2"),
						Key:         aws.String(time.Now().Format("20060102150405") + ".zip"),
						Body:        bytes.NewReader(file),
						ContentType: aws.String("application/zip"),
					})
					if err != nil {
						panic(err)
					}
				}
				api.PostMessage(ev.Channel, slack.MsgOptionText("Yes, hello.", false))
			}
		}
	})
	log.Fatal(http.ListenAndServe(":8080", nil))
}
