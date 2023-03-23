package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

var (
	slackClient     *slack.Client
	s3Client        *s3.Client
	s3PresignClient *s3.PresignClient
)

func init() {
	slackClient = slack.New(os.Getenv("SLACK_OAUTH_TOKEN"))

	sdkconfig, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		log.Println("初期設定中にエラーが発生しました。", err)
	}
	s3Client = s3.NewFromConfig(sdkconfig, func(o *s3.Options) {
		o.UsePathStyle = true
	})

	s3PresignClient = s3.NewPresignClient(s3Client)
}

type SlackAppMentionEventFiles struct {
	Files []SlackAppMentionEventFile `json:"files"`
}

type SlackAppMentionEventFile struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	URLPrivateDownload string `json:"url_private_download"`
}

func lambdaHandler(r events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	body := r.Body
	headers := r.Headers
	log.Println("リクエストヘッダー", headers)
	log.Println("リクエストボディ", body)

	// Slackのリトライリクエストは無視する。
	if headers["X-Slack-Retry-Num"] != "" {
		return events.APIGatewayProxyResponse{StatusCode: 200, Body: "No need retry"}, nil
	}

	// SlackAPIのシークレットキーを用いて検証する。
	signingSecret := os.Getenv("SLACK_SIGHNG_SECRET")
	header := http.Header{}
	for key, value := range headers {
		header.Set(key, value)
	}
	sv, err := slack.NewSecretsVerifier(header, signingSecret)
	if err != nil {
		log.Println("検証中にエラーが発生しました。", err)
		return events.APIGatewayProxyResponse{StatusCode: 400, Body: "Bad Request"}, err
	}
	if _, err := sv.Write([]byte(body)); err != nil {
		log.Println("検証中にエラーが発生しました。", err)
		return events.APIGatewayProxyResponse{StatusCode: 400, Body: "Bad Request"}, err
	}
	if err := sv.Ensure(); err != nil {
		return events.APIGatewayProxyResponse{StatusCode: 401, Body: "Unauthorized"}, err
	}

	eventsAPIEvent, err := slackevents.ParseEvent(json.RawMessage(body), slackevents.OptionNoVerifyToken())
	if err != nil {
		log.Println("リクエストの解析中にエラーが発生しました。", err)
		return events.APIGatewayProxyResponse{StatusCode: 500, Body: "Internal Server Error"}, err
	}

	// SlackAPIのURL検証用
	if eventsAPIEvent.Type == slackevents.URLVerification {
		var cr *slackevents.ChallengeResponse
		if err := json.Unmarshal([]byte(body), &cr); err != nil {
			log.Println("SlackAPIからのURL検証用中にエラーが発生しました。", err)
			return events.APIGatewayProxyResponse{StatusCode: 500, Body: "Internal Server Error"}, err
		}
		return events.APIGatewayProxyResponse{StatusCode: 200, Body: cr.Challenge}, nil
	}

	// メイン処理
	if eventsAPIEvent.Type == slackevents.CallbackEvent {
		innerEvent := eventsAPIEvent.InnerEvent
		switch ev := innerEvent.Data.(type) {
		case *slackevents.AppMentionEvent:
			var files *SlackAppMentionEventFiles
			if err := json.Unmarshal([]byte(innerEvent.Data.(string)), &files); err != nil {
				log.Println(innerEvent.Data.(string))
				log.Println("リクエストの解析中にエラーが発生しました。", err)
				return events.APIGatewayProxyResponse{StatusCode: 500, Body: "Internal Server Error"}, err
			}

			for _, file := range files.Files {
				url := file.URLPrivateDownload

				// Slackからファイルを取得する。
				res, err := http.Get(url)
				if err != nil {
					log.Println("Slackからファイルを取得中にエラーが発生しました。", err)
					continue
				}
				defer res.Body.Close()

				f, err := ioutil.ReadAll(res.Body)
				if err != nil {
					log.Println("Slackからファイルを取得中にエラーが発生しました。", err)
					continue
				}

				filename := time.Now().Format("20060102150405") + ".zip"

				// ファイルをS3にアップロードする。
				if _, err := s3Client.PutObject(context.TODO(), &s3.PutObjectInput{
					Bucket:      aws.String(os.Getenv("S3_BUCKET")),
					Key:         aws.String(filename),
					Body:        bytes.NewReader(f),
					ContentType: aws.String("application/zip"),
				}); err != nil {
					log.Println("ファイルをS3にアップロード中にエラーが発生しました。", err)
					continue
				}

				// 署名付きURLを生成する。
				pr, err := s3PresignClient.PresignGetObject(context.TODO(), &s3.GetObjectInput{
					Bucket: aws.String(os.Getenv("S3_BUCKET")),
					Key:    aws.String(filename),
				}, func(opts *s3.PresignOptions) {
					opts.Expires = time.Duration(60 * 3 * int64(time.Second))
				})
				if err != nil {
					log.Println("署名付きURLを生成中にエラーが発生しました。", err)
					continue
				}

				// Slackからファイルを削除する。
				if err := slackClient.DeleteFile(file.ID); err != nil {
					log.Println("ファイルの削除中にエラーが発生しました。", err)
				}

				// Slackにメッセージを送信する。
				if _, _, err := slackClient.PostMessage(
					ev.Channel,
					slack.MsgOptionText(pr.URL, false),
					slack.MsgOptionTS(ev.TimeStamp),
				); err != nil {
					log.Println("Slackにメッセージを送信中にエラーが発生しました。", err)
					continue
				}
			}
		}
	}

	return events.APIGatewayProxyResponse{StatusCode: 200, Body: "OK"}, nil
}

func main() {
	lambda.Start(lambdaHandler)
}
