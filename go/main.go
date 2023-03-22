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

	customResolver := aws.EndpointResolverFunc(func(service, region string) (aws.Endpoint, error) {
		return aws.Endpoint{
			PartitionID:   "aws",
			URL:           os.Getenv("S3_URL"),
			SigningRegion: os.Getenv("S3_REGION"),
		}, nil
	})
	sdkconfig, _ := config.LoadDefaultConfig(context.TODO(), config.WithEndpointResolver(customResolver))
	s3Client = s3.NewFromConfig(sdkconfig, func(o *s3.Options) {
		o.UsePathStyle = true
	})

	s3PresignClient = s3.NewPresignClient(s3Client)
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
			tsFrom, _ := strconv.ParseInt(ev.TimeStamp, 10, 64)
			tsTo, _ := strconv.ParseInt(ev.EventTimeStamp, 10, 64)

			// Slackからファイル情報を取得する。
			files, _, err := slackClient.GetFiles(slack.GetFilesParameters{
				User:          ev.User,
				Channel:       ev.Channel,
				TimestampFrom: slack.JSONTime(tsFrom),
				TimestampTo:   slack.JSONTime(tsTo),
			})
			if err != nil {
				log.Println("Slackからファイル情報を取得中にエラーが発生しました。", err)
				return events.APIGatewayProxyResponse{StatusCode: 400, Body: "Bad Request"}, err
			}

			for _, file := range files {
				url := file.URLPrivateDownload

				// Slackからファイルを取得する。
				res, err := http.Get(url)
				if err != nil {
					log.Println("Slackからファイルを取得中にエラーが発生しました。", err)
					continue
				}
				defer res.Body.Close()

				file, err := ioutil.ReadAll(res.Body)
				if err != nil {
					log.Println("Slackからファイルを取得中にエラーが発生しました。", err)
					continue
				}

				filename := time.Now().Format("20060102150405") + ".zip"

				// ファイルをS3にアップロードする。
				if _, err := s3Client.PutObject(context.TODO(), &s3.PutObjectInput{
					Bucket:      aws.String(os.Getenv("S3_BUCKET")),
					Key:         aws.String(filename),
					Body:        bytes.NewReader(file),
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
