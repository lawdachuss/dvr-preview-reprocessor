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
	baseURL  string
	username string
	password string
	client   *http.Client
}

func NewNodeDownloader(baseURL, username, password string) *NodeDownloader {
	return &NodeDownloader{
		baseURL:  strings.TrimRight(baseURL, "/"),
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
	u := fmt.Sprintf("%s/download?path=%s", d.baseURL, url.QueryEscape(filePath))

	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("User-Agent", "Mozilla/5.0")
	if d.username != "" || d.password != "" {
		req.SetBasicAuth(d.username, d.password)
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("file not found on node (404)")
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("node returned status %d", resp.StatusCode)
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
