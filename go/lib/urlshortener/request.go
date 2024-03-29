package urlshortener

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
)

type RequestBody struct {
	URL string `json:"url"`
}

type ResponseBody struct {
	URL string `json:"shortened_url"`
}

type URLShortener interface {
	Shorten(url string) (string, error)
}

type urlShortener struct {
}

func (r *urlShortener) Shorten(url string) (string, error) {
	endpoint := os.Getenv("URL_SHORTENER_URL")
	method := "POST"

	requestBody := RequestBody{
		URL: url,
	}
	requestBodyBytes, err := json.Marshal(requestBody)
	if err != nil {
		return "", fmt.Errorf("unable to marshal request body, %s", err)
	}

	request, err := http.NewRequestWithContext(context.TODO(), method, endpoint, bytes.NewBuffer(requestBodyBytes))
	if err != nil {
		return "", fmt.Errorf("unable to create new request, %s", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", os.Getenv("URL_SHORTENER_API_KEY"))

	client := &http.Client{}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("unable to send request, %s", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("request failed with status code %d", response.StatusCode)
	}

	responseBodyBytes, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return "", fmt.Errorf("unable to read response body, %s", err)
	}

	var responseBody ResponseBody
	err = json.Unmarshal(responseBodyBytes, &responseBody)
	if err != nil {
		return "", fmt.Errorf("unable to unmarshal response body, %s", err)
	}

	return responseBody.URL, nil
}

func NewURLShortener() URLShortener {
	return &urlShortener{}
}
