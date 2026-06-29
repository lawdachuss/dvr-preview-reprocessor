package download

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type StreamtapeDownloader struct {
	login  string
	key    string
	client *http.Client
}

func NewStreamtapeDownloader(login, key string) *StreamtapeDownloader {
	return &StreamtapeDownloader{
		login: login,
		key:   key,
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

type streamtapeDLTicketResp struct {
	Status int    `json:"status"`
	Msg    string `json:"msg"`
	Result struct {
		Ticket string `json:"ticket"`
	} `json:"result"`
}

type streamtapeDLResp struct {
	Status int    `json:"status"`
	Msg    string `json:"msg"`
	Result struct {
		URL string `json:"url"`
	} `json:"result"`
}

func (d *StreamtapeDownloader) Download(rawURL, destPath string) error {
	fileID := extractStreamtapeFileID(rawURL)
	if fileID == "" {
		return fmt.Errorf("could not extract file ID from URL: %s", rawURL)
	}

	ticket, err := d.getTicket(fileID)
	if err != nil {
		return fmt.Errorf("get ticket: %w", err)
	}

	directURL, err := d.getDirectURL(fileID, ticket)
	if err != nil {
		return fmt.Errorf("get direct URL: %w", err)
	}

	return d.downloadFile(directURL, destPath)
}

func extractStreamtapeFileID(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}

	path := strings.TrimRight(parsed.Path, "/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 {
		return ""
	}

	last := parts[len(parts)-1]
	if last == "" && len(parts) > 1 {
		last = parts[len(parts)-2]
	}

	last = strings.TrimSuffix(last, "/")
	return last
}

func (d *StreamtapeDownloader) getTicket(fileID string) (string, error) {
	apiURL := fmt.Sprintf("https://api.streamtape.com/file/dlticket?file=%s&login=%s&key=%s",
		fileID, d.login, d.key)

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return "", fmt.Errorf("create ticket request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := d.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("ticket request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read ticket response: %w", err)
	}

	var ticketResp streamtapeDLTicketResp
	if err := json.Unmarshal(body, &ticketResp); err != nil {
		return "", fmt.Errorf("decode ticket response: %w", err)
	}

	if ticketResp.Status != 200 {
		return "", fmt.Errorf("ticket API error %d: %s", ticketResp.Status, ticketResp.Msg)
	}

	if ticketResp.Result.Ticket == "" {
		return "", fmt.Errorf("empty ticket in response")
	}

	return ticketResp.Result.Ticket, nil
}

func (d *StreamtapeDownloader) getDirectURL(fileID, ticket string) (string, error) {
	apiURL := fmt.Sprintf("https://api.streamtape.com/file/dl?file=%s&ticket=%s", fileID, ticket)

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return "", fmt.Errorf("create dl request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")

	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("dl request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusMovedPermanently {
		location := resp.Header.Get("Location")
		if location != "" {
			return location, nil
		}
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read dl response: %w", err)
	}

	var dlResp streamtapeDLResp
	if err := json.Unmarshal(body, &dlResp); err != nil {
		return "", fmt.Errorf("decode dl response: %w", err)
	}

	if dlResp.Status != 200 {
		return "", fmt.Errorf("dl API error %d: %s", dlResp.Status, dlResp.Msg)
	}

	if dlResp.Result.URL == "" {
		return "", fmt.Errorf("empty direct URL in response")
	}

	return dlResp.Result.URL, nil
}

func (d *StreamtapeDownloader) downloadFile(directURL, destPath string) error {
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
