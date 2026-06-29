package download

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const gofileAPIBase = "https://api.gofile.io"

type GoFileDownloader struct {
	client   *http.Client
	apiToken string
}

func NewGoFileDownloader() *GoFileDownloader {
	return &GoFileDownloader{
		client: &http.Client{
			Timeout: 30 * time.Minute,
			Transport: &http.Transport{
				MaxIdleConns:        10,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 15 * time.Second,
			},
		},
	}
}

type gofileContentResp struct {
	Status string `json:"status"`
	Data   struct {
		Contents map[string]struct {
			Link string `json:"link"`
			Name string `json:"name"`
			Size int64  `json:"size"`
		} `json:"contents"`
	} `json:"data"`
}

func (d *GoFileDownloader) Download(rawURL, destPath string) error {
	contentID := extractGoFileContentID(rawURL)
	if contentID == "" {
		return fmt.Errorf("could not extract content ID from URL: %s", rawURL)
	}

	apiURL := fmt.Sprintf("%s/contents/%s", gofileAPIBase, contentID)
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Accept", "application/json")

	if d.apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+d.apiToken)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("contents request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read contents response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("contents API returned status %d: %s", resp.StatusCode, string(body))
	}

	var contentResp gofileContentResp
	if err := json.Unmarshal(body, &contentResp); err != nil {
		return fmt.Errorf("decode contents response: %w", err)
	}

	if contentResp.Status != "ok" {
		return fmt.Errorf("contents API status not ok: %s", contentResp.Status)
	}

	var directLink string
	for _, item := range contentResp.Data.Contents {
		if item.Link != "" {
			directLink = item.Link
			break
		}
	}

	if directLink == "" {
		return fmt.Errorf("no downloadable link found in GoFile contents")
	}

	return d.downloadFile(directLink, destPath)
}

func (d *GoFileDownloader) downloadFile(directURL, destPath string) error {
	out, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create output file: %w", err)
	}
	defer out.Close()

	req, err := http.NewRequest("GET", directURL, nil)
	if err != nil {
		return fmt.Errorf("create download request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("download request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	written, err := io.Copy(out, resp.Body)
	if err != nil {
		return fmt.Errorf("download write: %w", err)
	}

	if written == 0 {
		return fmt.Errorf("downloaded 0 bytes")
	}

	return nil
}

func extractGoFileContentID(rawURL string) string {
	rawURL = strings.TrimRight(rawURL, "/")

	if idx := strings.LastIndex(rawURL, "/"); idx >= 0 {
		return rawURL[idx+1:]
	}

	return rawURL
}
