package cli_util

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"go.uber.org/zap"
)

func isOk(statusCode int) bool {
	return statusCode >= 200 && statusCode < 300
}

// make a function that makes an http GET request to the daemon and returns data of type T
func GetRequest[T any](url string, logger *zap.SugaredLogger) (*T, error) {
	resp, err := http.Get(url)
	if err != nil {
		logger.Debugw("GET request failed", zap.String("url", url), zap.Error(err))
		return nil, fmt.Errorf("http get request failed: %v", err)
	}
	defer resp.Body.Close()

	logger.Debugw("GET request", zap.String("url", url), resp)

	if !isOk(resp.StatusCode) {
		responseBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("error reading response body: %v", err)
		}

		responseBody = []byte(strings.TrimSuffix(string(responseBody), "\n"))

		return nil, fmt.Errorf("http get request failed: %s", responseBody)
	}

	var data T
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("failed to decode http response: %v", err)
	}

	return &data, nil
}

func DeleteRequest(url string, logger *zap.SugaredLogger) error {
	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return fmt.Errorf("failed to delete: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to delete: %v", err)
	}
	defer resp.Body.Close()

	logger.Debugw("DELETE request", zap.String("url", url), resp)

	if !isOk(resp.StatusCode) {
		responseBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("error reading response body: %v", err)
		}

		responseBody = []byte(strings.TrimSuffix(string(responseBody), "\n"))

		return fmt.Errorf("delete failed: %s", responseBody)
	}

	return nil
}

func PutRequest(url string, data io.Reader, logger *zap.SugaredLogger) error {
	req, err := http.NewRequest("PUT", url, data)
	if err != nil {
		return fmt.Errorf("failed to put: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to put: %v", err)
	}
	defer resp.Body.Close()

	logger.Debugw("PUT request", zap.String("url", url), resp)

	if !isOk(resp.StatusCode) {
		responseBody, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("error reading response body: %v", err)
		}

		responseBody = []byte(strings.TrimSuffix(string(responseBody), "\n"))

		return fmt.Errorf("put failed: %s", responseBody)
	}

	return nil
}
