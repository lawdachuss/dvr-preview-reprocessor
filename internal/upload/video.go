package upload

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ─── Catbox ───────────────────────────────────────────────────────────────────

type CatboxUploader struct {
	client   *http.Client
	userhash string
}

func NewCatboxUploader() *CatboxUploader {
	return &CatboxUploader{
		client: &http.Client{
			Timeout: 5 * time.Minute,
			Transport: &http.Transport{
				MaxIdleConns:          100,
				MaxIdleConnsPerHost:   10,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   15 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
			},
		},
		userhash: os.Getenv("CATBOX_USERHASH"),
	}
}

func (u *CatboxUploader) Upload(filePath string) (string, error) {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(1<<uint(attempt)) * time.Second)
		}
		url, err := u.uploadOnce(filePath)
		if err == nil {
			return url, nil
		}
		lastErr = err
		if isRetryableCatboxError(err) {
			continue
		}
		return "", err
	}
	return "", fmt.Errorf("catbox: all 5 attempts failed, last: %w", lastErr)
}

func mimeTypeFor(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".mp4":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".webp":
		return "image/webp"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	default:
		return "application/octet-stream"
	}
}

func (u *CatboxUploader) uploadOnce(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("catbox: open file: %w", err)
	}
	defer file.Close()

	pipeReader, pipeWriter := io.Pipe()
	writer := multipart.NewWriter(pipeWriter)

	errChan := make(chan error, 1)
	go func() {
		defer pipeWriter.Close()
		defer writer.Close()

		if err := writer.WriteField("reqtype", "fileupload"); err != nil {
			errChan <- fmt.Errorf("write reqtype: %w", err)
			return
		}
		if u.userhash != "" {
			if err := writer.WriteField("userhash", u.userhash); err != nil {
				errChan <- fmt.Errorf("write userhash: %w", err)
				return
			}
		}

		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition",
			fmt.Sprintf(`form-data; name="fileToUpload"; filename="%s"`, filepath.Base(filePath)))
		h.Set("Content-Type", mimeTypeFor(filePath))
		part, err := writer.CreatePart(h)
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

	req, err := http.NewRequest("POST", "https://catbox.moe/user/api.php", pipeReader)
	if err != nil {
		pipeReader.CloseWithError(err)
		select {
		case <-errChan:
		case <-time.After(5 * time.Second):
		}
		return "", fmt.Errorf("catbox: create request: %w", err)
	}

	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("User-Agent", defaultUserAgent)

	resp, err := u.client.Do(req)
	if err != nil {
		pipeReader.CloseWithError(err)
		select {
		case <-errChan:
		case <-time.After(5 * time.Second):
		}
		return "", fmt.Errorf("catbox: send request: %w", err)
	}
	defer resp.Body.Close()

	select {
	case err := <-errChan:
		if err != nil {
			return "", err
		}
	case <-time.After(30 * time.Second):
		return "", fmt.Errorf("catbox: timeout waiting for file copy to complete")
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", fmt.Errorf("catbox: read response: %w", err)
	}

	text := strings.TrimSpace(string(raw))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("catbox: status %d: %s", resp.StatusCode, text)
	}
	if text == "" {
		return "", fmt.Errorf("catbox: empty response")
	}
	if !strings.HasPrefix(text, "https://") {
		return "", fmt.Errorf("catbox: unexpected response: %s", text)
	}
	return text, nil
}

func isRetryableCatboxError(err error) bool {
	errStr := err.Error()
	if strings.Contains(errStr, "status 5") {
		return true
	}
	if strings.Contains(errStr, "Invalid uploader") {
		return true
	}
	if strings.Contains(errStr, "stat file") || strings.Contains(errStr, "open file") {
		return true
	}
	if strings.Contains(errStr, "send request") || strings.Contains(errStr, "read response") {
		return true
	}
	return false
}

// ─── PixelDrain ───────────────────────────────────────────────────────────────

type PixelDrainUploader struct {
	apiKey string
	client *http.Client
}

func NewPixelDrainUploader(apiKey string) *PixelDrainUploader {
	return &PixelDrainUploader{
		apiKey: apiKey,
		client: newNoProxyClient(5 * time.Minute),
	}
}

type pixelDrainUploadResponse struct {
	ID      string `json:"id"`
	Success bool   `json:"success"`
}

func (u *PixelDrainUploader) Upload(filePath string) (string, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(1<<uint(attempt)) * time.Second)
		}
		url, err := u.uploadOnce(filePath)
		if err == nil {
			return url, nil
		}
		lastErr = err
		if isRetryablePixelDrainError(err) {
			continue
		}
		return "", err
	}
	return "", fmt.Errorf("pixeldrain: all 3 attempts failed, last: %w", lastErr)
}

func (u *PixelDrainUploader) uploadOnce(filePath string) (string, error) {
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)

	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("pixeldrain: open file: %w", err)
	}
	defer file.Close()

	part, err := mw.CreateFormFile("file", filepath.Base(filePath))
	if err != nil {
		mw.Close()
		return "", fmt.Errorf("pixeldrain: create form file: %w", err)
	}

	if _, err := io.Copy(part, file); err != nil {
		mw.Close()
		return "", fmt.Errorf("pixeldrain: copy file: %w", err)
	}
	mw.Close()

	req, err := http.NewRequest("POST", "https://pixeldrain.com/api/file", &b)
	if err != nil {
		return "", fmt.Errorf("pixeldrain: create request: %w", err)
	}

	req.ContentLength = int64(b.Len())
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("User-Agent", defaultUserAgent)
	if u.apiKey != "" {
		req.SetBasicAuth("", u.apiKey)
	}

	resp, err := u.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("pixeldrain: send request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", fmt.Errorf("pixeldrain: read response: %w", err)
	}

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("pixeldrain: status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var uploadResp pixelDrainUploadResponse
	if err := json.Unmarshal(raw, &uploadResp); err != nil {
		return "", fmt.Errorf("pixeldrain: decode response: %w", err)
	}

	if !uploadResp.Success || uploadResp.ID == "" {
		return "", fmt.Errorf("pixeldrain: upload failed: %s", strings.TrimSpace(string(raw)))
	}

	return fmt.Sprintf("https://pixeldrain.com/api/file/%s", uploadResp.ID), nil
}

func isRetryablePixelDrainError(err error) bool {
	errStr := err.Error()
	if strings.HasPrefix(errStr, "pixeldrain: status 5") {
		return true
	}
	if strings.Contains(errStr, "stat file") || strings.Contains(errStr, "open file") {
		return true
	}
	if strings.Contains(errStr, "send request") || strings.Contains(errStr, "read response") {
		return true
	}
	return false
}

// ─── LobFile ──────────────────────────────────────────────────────────────────

type LobFileUploader struct {
	apiKey string
	client *http.Client
}

func NewLobFileUploader(apiKey string) *LobFileUploader {
	return &LobFileUploader{
		apiKey: apiKey,
		client: &http.Client{
			Timeout: 10 * time.Minute,
			Transport: &http.Transport{
				MaxIdleConns:          10,
				MaxIdleConnsPerHost:   4,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   15 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
				DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
			},
		},
	}
}

type lobFileUploadResponse struct {
	Success bool   `json:"success"`
	URL     string `json:"url"`
	Error   string `json:"error,omitempty"`
}

func (u *LobFileUploader) Upload(filePath string) (string, error) {
	if u.apiKey == "" {
		return "", fmt.Errorf("lobfile: API key not set (set LOBFILE_API_KEY env var)")
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(1<<uint(attempt)) * time.Second)
		}
		url, err := u.uploadOnce(filePath)
		if err == nil {
			return url, nil
		}
		lastErr = err
		if isRetryableLobFileError(err) {
			continue
		}
		return "", err
	}
	return "", fmt.Errorf("lobfile: all 3 attempts failed, last: %w", lastErr)
}

func (u *LobFileUploader) uploadOnce(filePath string) (string, error) {
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)

	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("lobfile: open file: %w", err)
	}
	defer file.Close()

	part, err := mw.CreateFormFile("file", filepath.Base(filePath))
	if err != nil {
		mw.Close()
		return "", fmt.Errorf("lobfile: create form file: %w", err)
	}

	if _, err := io.Copy(part, file); err != nil {
		mw.Close()
		return "", fmt.Errorf("lobfile: copy file: %w", err)
	}
	mw.Close()

	req, err := http.NewRequest("POST", "https://lobfile.com/api/v3/upload", &b)
	if err != nil {
		return "", fmt.Errorf("lobfile: create request: %w", err)
	}

	req.ContentLength = int64(b.Len())
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("X-API-Key", u.apiKey)

	resp, err := u.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("lobfile: send request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
	if err != nil {
		return "", fmt.Errorf("lobfile: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("lobfile: status %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var result lobFileUploadResponse
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", fmt.Errorf("lobfile: decode response: %w", err)
	}

	if !result.Success {
		errMsg := result.Error
		if errMsg == "" {
			errMsg = string(raw)
		}
		return "", fmt.Errorf("lobfile: upload failed: %s", errMsg)
	}

	if result.URL == "" {
		return "", fmt.Errorf("lobfile: empty URL in response")
	}

	return result.URL, nil
}

func isRetryableLobFileError(err error) bool {
	errStr := err.Error()
	if strings.Contains(errStr, "status 5") {
		return true
	}
	if strings.Contains(errStr, "stat file") || strings.Contains(errStr, "open file") {
		return true
	}
	if strings.Contains(errStr, "send request") || strings.Contains(errStr, "read response") {
		return true
	}
	return false
}
