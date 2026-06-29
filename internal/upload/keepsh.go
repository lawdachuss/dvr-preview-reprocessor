package upload

import (
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type KeepShUploader struct {
	client *http.Client
}

func NewKeepShUploader() *KeepShUploader {
	return &KeepShUploader{
		client: &http.Client{
			Timeout: 5 * time.Minute,
		},
	}
}

func (u *KeepShUploader) Upload(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("keep.sh: open file: %w", err)
	}
	defer file.Close()

	pipeReader, pipeWriter := io.Pipe()
	writer := multipart.NewWriter(pipeWriter)

	errChan := make(chan error, 1)
	go func() {
		defer pipeWriter.Close()
		defer writer.Close()

		part, err := writer.CreateFormFile("file", filepath.Base(filePath))
		if err != nil {
			errChan <- fmt.Errorf("create form file: %w", err)
			return
		}

		if _, err := io.Copy(part, file); err != nil {
			errChan <- fmt.Errorf("copy file: %w", err)
			return
		}
		errChan <- nil
	}()

	req, err := http.NewRequest("POST", "https://keep.sh/", pipeReader)
	if err != nil {
		pipeReader.CloseWithError(err)
		select {
		case <-errChan:
		case <-time.After(5 * time.Second):
		}
		return "", fmt.Errorf("keep.sh: create request: %w", err)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Accept", "text/plain")

	resp, err := u.client.Do(req)
	if err != nil {
		pipeReader.CloseWithError(err)
		select {
		case <-errChan:
		case <-time.After(5 * time.Second):
		}
		return "", fmt.Errorf("keep.sh: send request: %w", err)
	}
	defer resp.Body.Close()

	select {
	case err := <-errChan:
		if err != nil {
			return "", err
		}
	case <-time.After(30 * time.Second):
		return "", fmt.Errorf("keep.sh: timeout waiting for file copy")
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	if err != nil {
		return "", fmt.Errorf("keep.sh: read response: %w", err)
	}

	text := strings.TrimSpace(string(raw))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("keep.sh: status %d: %s", resp.StatusCode, text)
	}
	if text == "" {
		return "", fmt.Errorf("keep.sh: empty response")
	}
	if !strings.HasPrefix(text, "https://") {
		return "", fmt.Errorf("keep.sh: unexpected response: %s", text)
	}
	return text, nil
}
