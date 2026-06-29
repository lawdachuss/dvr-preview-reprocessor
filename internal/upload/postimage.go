package upload

import (
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type PostImageUploader struct {
	client *http.Client
}

type postImageResponse struct {
	Status string `json:"status"`
	Image  *struct {
		URL string `json:"url"`
	} `json:"image"`
	URL string `json:"url"`
}

func NewPostImageUploader() *PostImageUploader {
	return &PostImageUploader{
		client: &http.Client{
			Timeout: 2 * time.Minute,
		},
	}
}

func (u *PostImageUploader) Upload(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("postimage: open file: %w", err)
	}
	defer file.Close()

	var b strings.Builder
	mw := multipart.NewWriter(&b)

	if err := mw.WriteField("type", "file"); err != nil {
		return "", fmt.Errorf("postimage: write type: %w", err)
	}
	if err := mw.WriteField("adult", "no"); err != nil {
		return "", fmt.Errorf("postimage: write adult: %w", err)
	}

	part, err := mw.CreateFormFile("file", filepath.Base(filePath))
	if err != nil {
		return "", fmt.Errorf("postimage: create form file: %w", err)
	}

	if _, err := io.Copy(part, file); err != nil {
		mw.Close()
		return "", fmt.Errorf("postimage: copy file: %w", err)
	}
	mw.Close()

	req, err := http.NewRequest("POST", "https://postimages.org/json/rr", strings.NewReader(b.String()))
	if err != nil {
		return "", fmt.Errorf("postimage: create request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := u.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("postimage: send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", fmt.Errorf("postimage: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("postimage: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result postImageResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("postimage: decode response: %w", err)
	}

	if result.Status != "OK" {
		return "", fmt.Errorf("postimage: API error: %s", string(body))
	}

	if result.Image != nil && result.Image.URL != "" {
		return result.Image.URL, nil
	}

	return "", fmt.Errorf("postimage: no image URL in response: %s", string(body))
}
