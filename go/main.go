package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/kumagai-s/uploader-v2/lib/urlshortener"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

var (
	slackClientAsBot  *slack.Client
	slackClientAsUser *slack.Client
	s3Client          *s3.Client
	s3PresignClient   *s3.PresignClient
)

func init() {
	slackClientAsBot = slack.New(os.Getenv("SLACK_BOT_OAUTH_TOKEN"))
	slackClientAsUser = slack.New(os.Getenv("SLACK_USER_OAUTH_TOKEN"))

	cred := aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(
		os.Getenv("AWS_ACCESS_KEY_ID"),
		os.Getenv("AWS_SECRET_ACCESS_KEY"),
		"",
	))

	sdkconfig, err := config.LoadDefaultConfig(context.TODO(), config.WithCredentialsProvider(cred))
	if err != nil {
		log.Println("初期設定中にエラーが発生しました。", err)
	}
	s3Client = s3.NewFromConfig(sdkconfig, func(o *s3.Options) {
		o.UsePathStyle = true
	})

	s3PresignClient = s3.NewPresignClient(s3Client)
}

type SlackAppMentionEventRequest struct {
	Event SlackAppMentionEvent `json:"event"`
}

type SlackAppMentionEvent struct {
	Files []SlackAppMentionEventFile `json:"files"`
}

type SlackAppMentionEventFile struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	URLPrivateDownload string `json:"url_private_download"`
	Binary             []byte // Slackからファイルを取得した際、取得したファイルのバイナリデータが格納されます。
}

// verifyRequest は、SlackAPIからのリクエストが正当なものかどうかを検証します。
// 検証にはシークレットキーを使用し、正当性を確認します。
// headers: SlackAPIから受信したリクエストヘッダー
// body: SlackAPIから受信したリクエストボディ
// エラーがなければnilを返し、検証に失敗した場合はエラーを返します。
func verifyRequest(headers map[string]string, body string) error {
	signingSecret := os.Getenv("SLACK_SIGHNG_SECRET")
	header := http.Header{}
	for key, value := range headers {
		header.Set(key, value)
	}
	sv, err := slack.NewSecretsVerifier(header, signingSecret)
	if err != nil {
		return err
	}
	if _, err := sv.Write([]byte(body)); err != nil {
		return err
	}
	if err := sv.Ensure(); err != nil {
		return err
	}
	return nil
}

// handleURLVerification は、Slack APIからのURL検証リクエストを処理します。
// body: SlackAPIから受信したリクエストボディ
// URL検証リクエストが正常に処理された場合、APIGatewayProxyResponseとnilのエラーを返します。
// エラーが発生した場合、適切なAPIGatewayProxyResponseとエラーを返します。
func handleURLVerification(body string) (events.APIGatewayProxyResponse, error) {
	var cr *slackevents.ChallengeResponse
	if err := json.Unmarshal([]byte(body), &cr); err != nil {
		log.Println("SlackAPIからのURL検証用中にエラーが発生しました。", err)
		return events.APIGatewayProxyResponse{StatusCode: 500, Body: "Internal Server Error"}, err
	}
	return events.APIGatewayProxyResponse{StatusCode: 200, Body: cr.Challenge}, nil
}

// uploadFileToS3AndGetPresignedURL は、Slackから取得したファイルをS3にアップロードし、
// 署名付きURLを生成して返します。
// file: アップロードするSlackファイルオブジェクトへのポインタ
// 成功時には署名付きURLの文字列とnilのエラーを返します。
// エラーが発生した場合、空文字列とエラーを返します。
func uploadFileToS3AndGetPresignedURL(file *SlackAppMentionEventFile) (string, error) {
	// ファイルをS3にアップロードする。
	if _, err := s3Client.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket:      aws.String(os.Getenv("S3_BUCKET")),
		Key:         aws.String(file.Name),
		Body:        bytes.NewReader(file.Binary),
		ContentType: aws.String("application/zip"),
	}); err != nil {
		return "", err
	}

	// 署名付きURLを生成する。
	pr, err := s3PresignClient.PresignGetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: aws.String(os.Getenv("S3_BUCKET")),
		Key:    aws.String(file.Name),
	}, func(opts *s3.PresignOptions) {
		opts.Expires = time.Duration(60 * 60 * 24 * 7 * int64(time.Second))
	})
	if err != nil {
		return "", err
	}

	return pr.URL, nil
}

// validateFile は、指定された SlackAppMentionEventFile が以下の条件を満たすか確認します。
// ・ファイルが zip 形式であること
// ・ファイル名が半角英数字であること
// 条件を満たさない場合はエラーを返します。
func validateFile(file *SlackAppMentionEventFile) error {
	isValidName := regexp.MustCompile(`^[a-zA-Z0-9_-]+$`).MatchString
	if !isValidName(file.Name[:len(file.Name)-4]) {
		return errors.New("ファイル名は「半角英数字」にしてください。")
	}

	if !strings.HasSuffix(file.Name, ".zip") {
		return errors.New("ファイルは「zip」形式にしてください。")
	}

	return nil
}

// sendErrorToSlack は、エラーメッセージをSlackのチャンネルに送信します。
// ev: AppMentionEventオブジェクトへのポインタ。エラーが発生したイベント情報を含む。
// 関数はエラーの送信成功時と失敗時の両方で、何も返しません。
func sendErrorToSlack(ev *slackevents.AppMentionEvent, errorMessage string) {
	if _, _, err := slackClientAsBot.PostMessage(
		ev.Channel,
		slack.MsgOptionText(errorMessage, false),
		slack.MsgOptionTS(ev.TimeStamp),
	); err != nil {
		log.Println("エラーメッセージをSlackに送信中にエラーが発生しました。", err)
	}
}

// handleAppMentionEvent は、AppMentionイベントを処理します。
// この関数は、SlackファイルをS3にアップロードし、署名付きURLを生成してSlackチャンネルに送信します。
// 最後に、アップロードされたファイルをSlackから削除します。
// ev: AppMentionイベントへのポインタ。イベント情報を含む。
// body: SlackAPIから受信したリクエストボディ
// AppMentionイベントが正常に処理された場合、APIGatewayProxyResponseとnilのエラーを返します。
// エラーが発生した場合、エラーメッセージをSlackチャンネルに送信し、適切なAPIGatewayProxyResponseとエラーを返します。
func handleAppMentionEvent(ev *slackevents.AppMentionEvent, body string) (events.APIGatewayProxyResponse, error) {
	var req *SlackAppMentionEventRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		sendErrorToSlack(ev, "エラーが発生しました。処理を完了できませんでした。")
		return events.APIGatewayProxyResponse{StatusCode: 500, Body: "Internal Server Error"}, err
	}

	for _, file := range req.Event.Files {
		// Slackからファイルを取得する。
		var buf bytes.Buffer

		if err := slackClientAsBot.GetFile(file.URLPrivateDownload, &buf); err != nil {
			log.Println("Slackからファイルを取得中にエラーが発生しました。", err)
			sendErrorToSlack(ev, "エラーが発生しました。処理を完了できませんでした。")
			return events.APIGatewayProxyResponse{StatusCode: 500, Body: "Internal Server Error"}, err
		}

		file.Binary = buf.Bytes()

		// Slackからファイルを削除する。
		if err := slackClientAsUser.DeleteFile(file.ID); err != nil {
			log.Println("Slackからファイルを削除中にエラーが発生しました。", err)
			sendErrorToSlack(ev, "エラーが発生しました。処理を完了できませんでした。")
			return events.APIGatewayProxyResponse{StatusCode: 500, Body: "Internal Server Error"}, err
		}

		if err := validateFile(&file); err != nil {
			sendErrorToSlack(ev, err.Error())
			return events.APIGatewayProxyResponse{StatusCode: 400, Body: "Bad Request"}, err
		}

		presignedURL, err := uploadFileToS3AndGetPresignedURL(&file)
		if err != nil {
			log.Println("ファイルのアップロードと署名付きURLの生成中にエラーが発生しました。", err)
			sendErrorToSlack(ev, "エラーが発生しました。処理を完了できませんでした。")
			return events.APIGatewayProxyResponse{StatusCode: 500, Body: "Internal Server Error"}, err
		}

		urlShortener := urlshortener.NewURLShortener()

		shortURL, err := urlShortener.Shorten(presignedURL)
		if err != nil {
			log.Println("URLの短縮中にエラーが発生しました。", err)
			sendErrorToSlack(ev, "URLの短縮中にエラーが発生しました。処理を完了できませんでした。")
			return events.APIGatewayProxyResponse{StatusCode: 500, Body: "Internal Server Error"}, err
		}

		// Slackにメッセージを送信する。
		if _, _, err := slackClientAsBot.PostMessage(
			ev.Channel,
			slack.MsgOptionText(shortURL, false),
			slack.MsgOptionTS(ev.TimeStamp),
		); err != nil {
			log.Println("Slackにメッセージを送信中にエラーが発生しました。", err)
			return events.APIGatewayProxyResponse{StatusCode: 500, Body: "Internal Server Error"}, err
		}
	}

	return events.APIGatewayProxyResponse{StatusCode: 200, Body: "OK"}, nil
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
	if err := verifyRequest(headers, body); err != nil {
		log.Println("リクエストの検証中にエラーが発生しました。", err)
		return events.APIGatewayProxyResponse{StatusCode: 401, Body: "Unauthorized"}, err
	}

	eventsAPIEvent, err := slackevents.ParseEvent(json.RawMessage(body), slackevents.OptionNoVerifyToken())
	if err != nil {
		log.Println("リクエストの解析中にエラーが発生しました。", err)
		return events.APIGatewayProxyResponse{StatusCode: 500, Body: "Internal Server Error"}, err
	}

	// SlackAPIのURL検証イベントを処理する。
	if eventsAPIEvent.Type == slackevents.URLVerification {
		return handleURLVerification(body)
	}

	// SlackAPIのコールバックイベント処理する。
	if eventsAPIEvent.Type == slackevents.CallbackEvent {
		innerEvent := eventsAPIEvent.InnerEvent
		switch ev := innerEvent.Data.(type) {
		case *slackevents.AppMentionEvent:
			return handleAppMentionEvent(ev, body)
		}
	}

	return events.APIGatewayProxyResponse{StatusCode: 400, Body: "Bad Request"}, err
}

func main() {
	lambda.Start(lambdaHandler)
}
