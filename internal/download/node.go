package download

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type NodeDownloader struct {
	nodes    []string
	username string
	password string
	client   *http.Client
}

func NewNodeDownloader(nodeURLs []string, username, password string) *NodeDownloader {
	cleaned := make([]string, len(nodeURLs))
	for i, u := range nodeURLs {
		cleaned[i] = strings.TrimRight(u, "/")
	}
	return &NodeDownloader{
		nodes:    cleaned,
		username: username,
		password: password,
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

func (d *NodeDownloader) Download(filePath, destPath string) error {
	var lastErr error
	for _, nodeURL := range d.nodes {
		u := fmt.Sprintf("%s/download?path=%s", nodeURL, url.QueryEscape(filePath))

		req, err := http.NewRequest("GET", u, nil)
		if err != nil {
			lastErr = fmt.Errorf("create request: %w", err)
			continue
		}

		req.Header.Set("User-Agent", "Mozilla/5.0")
		if d.username != "" || d.password != "" {
			req.SetBasicAuth(d.username, d.password)
		}

		resp, err := d.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("request to %s: %w", nodeURL, err)
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusNotFound {
			lastErr = fmt.Errorf("file not found on %s (404)", nodeURL)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("%s returned status %d", nodeURL, resp.StatusCode)
			continue
		}

		out, err := os.Create(destPath)
		if err != nil {
			return fmt.Errorf("create output file: %w", err)
		}
		defer out.Close()

		written, err := io.Copy(out, resp.Body)
		if err != nil {
			return fmt.Errorf("download write: %w", err)
		}

		if written == 0 {
			return fmt.Errorf("downloaded 0 bytes")
		}

		return nil
	}

	return fmt.Errorf("all nodes failed: last error: %w", lastErr)
}
