package download

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type VoeSXDownloader struct {
	client *http.Client
}

func NewVoeSXDownloader() *VoeSXDownloader {
	return &VoeSXDownloader{
		client: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        10,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 15 * time.Second,
			},
		},
	}
}



func (d *VoeSXDownloader) Download(rawURL, destPath string) error {
	embedID := extractVoeSXEmbedID(rawURL)
	if embedID == "" {
		return fmt.Errorf("could not extract embed ID from URL: %s", rawURL)
	}

	hlsURL, err := d.extractHLSURL(embedID)
	if err != nil {
		return fmt.Errorf("extract HLS URL: %w", err)
	}

	if !strings.Contains(hlsURL, ".m3u8") {
		return fmt.Errorf("extracted URL is not an HLS playlist: %s", hlsURL)
	}

	return d.downloadHLS(hlsURL, destPath)
}

func extractVoeSXEmbedID(rawURL string) string {
	rawURL = strings.TrimRight(rawURL, "/")

	parts := strings.Split(rawURL, "/")
	if len(parts) == 0 {
		return ""
	}

	last := parts[len(parts)-1]
	last = strings.Split(last, "?")[0]
	return last
}

func (d *VoeSXDownloader) extractHLSURL(embedID string) (string, error) {
	pageURL := fmt.Sprintf("https://voe.sx/e/%s", embedID)

	req, err := http.NewRequest("GET", pageURL, nil)
	if err != nil {
		return "", fmt.Errorf("create embed request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Referer", "https://voe.sx/")

	resp, err := d.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("embed request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read embed page: %w", err)
	}

	html := string(body)

	hlsURL := extractHLSFromPage(html)
	if hlsURL != "" {
		return hlsURL, nil
	}

	hlsURL = extractHLSFromJSON(html)
	if hlsURL != "" {
		return hlsURL, nil
	}

	return "", fmt.Errorf("could not find HLS URL in VOE.sx embed page")
}

func extractHLSFromPage(html string) string {
	markers := []string{".m3u8", "master.m3u8", "index.m3u8"}
	startMarkers := []string{"https://", "http://", "//"}

	idx := -1
	for _, marker := range markers {
		if i := strings.Index(html, marker); i >= 0 {
			idx = i
			break
		}
	}
	if idx < 0 {
		return ""
	}

	end := idx + len(".m3u8")

	start := -1
	for _, sm := range startMarkers {
		candidateStart := strings.LastIndex(html[:idx], sm)
		if candidateStart > start {
			start = candidateStart
		}
	}
	if start < 0 {
		return ""
	}

	result := html[start:end]
	if strings.HasPrefix(result, "//") {
		result = "https:" + result
	}

	return result
}

func extractHLSFromJSON(html string) string {
	startMarker := `"hls":"`
	idx := strings.Index(html, startMarker)
	if idx < 0 {
		return ""
	}

	start := idx + len(startMarker)
	end := strings.Index(html[start:], `"`)
	if end < 0 {
		return ""
	}

	hlsURL := html[start : start+end]
	hlsURL = strings.ReplaceAll(hlsURL, "\\/", "/")
	hlsURL = strings.ReplaceAll(hlsURL, "\\u002F", "/")

	if strings.HasPrefix(hlsURL, "//") {
		hlsURL = "https:" + hlsURL
	}

	return hlsURL
}

func (d *VoeSXDownloader) downloadHLS(hlsURL, destPath string) error {
	ext := filepath.Ext(destPath)
	if ext == "" {
		destPath += ".mp4"
	}

	args := []string{
		"-y",
		"-user_agent", "Mozilla/5.0",
		"-headers", "Referer: https://voe.sx/\r\n",
		"-i", hlsURL,
		"-c", "copy",
		"-bsf:a", "aac_adtstoasc",
		"-movflags", "+faststart",
		destPath,
	}

	cmd := exec.Command("ffmpeg", args...)
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg HLS download failed: %w", err)
	}

	fi, err := os.Stat(destPath)
	if err != nil {
		return fmt.Errorf("stat output file: %w", err)
	}
	if fi.Size() < 1024 {
		return fmt.Errorf("downloaded file too small: %d bytes", fi.Size())
	}

	return nil
}


