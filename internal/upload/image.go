package upload

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36"

func newNoProxyClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
	}
}

type MultiImageUploader struct {
	catbox    *CatboxUploader
	pixhost   *PixhostUploader
	imgbb     *ImgBBUploader
	postimage *PostImageUploader
}

func NewMultiImageUploader() *MultiImageUploader {
	return &MultiImageUploader{
		catbox:    NewCatboxUploader(),
		pixhost:   &PixhostUploader{},
		imgbb:     NewImgBBUploader(),
		postimage: NewPostImageUploader(),
	}
}

func uploadWithRetries(maxAttempts int, label string, fn func() (string, error)) (string, error) {
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(1<<attempt) * time.Second)
		}
		u, e := fn()
		if e == nil {
			return u, nil
		}
		lastErr = e
	}
	return "", lastErr
}

func (m *MultiImageUploader) Upload(filePath string) (url, host string, err error) {
	url, err = uploadWithRetries(2, "Catbox", func() (string, error) {
		return m.catbox.Upload(filePath)
	})
	if err == nil {
		return url, "Catbox", nil
	}

	url, err = uploadWithRetries(2, "Pixhost", func() (string, error) {
		return m.pixhost.Upload(filePath)
	})
	if err == nil {
		return url, "Pixhost", nil
	}

	url, err = uploadWithRetries(2, "ImgBB", func() (string, error) {
		return m.imgbb.Upload(filePath)
	})
	if err == nil {
		return url, "ImgBB", nil
	}

	url, err = uploadWithRetries(1, "PostImage", func() (string, error) {
		return m.postimage.Upload(filePath)
	})
	if err == nil {
		return url, "PostImage", nil
	}

	return "", "", fmt.Errorf("all image hosts failed")
}

// ─── Pixhost ──────────────────────────────────────────────────────────────────

type PixhostUploader struct {
	client *http.Client
}

type pixhostResponse struct {
	Name    string `json:"name"`
	ShowURL string `json:"show_url"`
	ThURL   string `json:"th_url"`
	ImgURL  string `json:"img_url"`
}

func (t *PixhostUploader) Upload(thumbnailPath string) (string, error) {
	if t.client == nil {
		t.client = newNoProxyClient(2 * time.Minute)
	}

	fi, err := os.Stat(thumbnailPath)
	if err != nil {
		return "", fmt.Errorf("stat file: %w", err)
	}
	fileSize := fi.Size()

	file, err := os.Open(thumbnailPath)
	if err != nil {
		return "", fmt.Errorf("open file: %w", err)
	}
	defer file.Close()

	var preamble bytes.Buffer
	mw := multipart.NewWriter(&preamble)
	if err := mw.WriteField("content_type", "1"); err != nil {
		return "", fmt.Errorf("write content_type: %w", err)
	}
	if err := mw.WriteField("max_th_size", "420"); err != nil {
		return "", fmt.Errorf("write max_th_size: %w", err)
	}
	if _, err := mw.CreateFormFile("img", filepath.Base(thumbnailPath)); err != nil {
		return "", fmt.Errorf("create form file: %w", err)
	}
	closing := fmt.Sprintf("\r\n--%s--\r\n", mw.Boundary())
	contentType := mw.FormDataContentType()

	ext := strings.ToLower(filepath.Ext(thumbnailPath))
	mimeType := "application/octet-stream"
	switch ext {
	case ".webp":
		mimeType = "image/webp"
	case ".jpg", ".jpeg":
		mimeType = "image/jpeg"
	case ".png":
		mimeType = "image/png"
	}

	multipartBody := func() io.Reader {
		file.Seek(0, io.SeekStart)
		return io.MultiReader(
			bytes.NewReader(preamble.Bytes()),
			file,
			bytes.NewReader([]byte(closing)),
		)
	}
	rawBody := func() io.Reader {
		file.Seek(0, io.SeekStart)
		return file
	}

	var lastErr error
	type strategy struct {
		name string
		fn   func() (*http.Response, error)
	}
	strategies := []strategy{
		{"multipart", func() (*http.Response, error) {
			totalLen := int64(preamble.Len()) + fileSize + int64(len(closing))
			req, err := http.NewRequest("POST", "https://api.pixhost.to/images", multipartBody())
			if err != nil {
				return nil, fmt.Errorf("create request: %w", err)
			}
			req.ContentLength = totalLen
			req.Header.Set("Content-Type", contentType)
			req.Header.Set("Accept", "application/json")
			req.Header.Set("User-Agent", defaultUserAgent)
			return t.client.Do(req)
		}},
		{"raw body", func() (*http.Response, error) {
			req, err := http.NewRequest("POST", "https://api.pixhost.to/images", rawBody())
			if err != nil {
				return nil, fmt.Errorf("create raw request: %w", err)
			}
			req.ContentLength = fileSize
			req.Header.Set("Content-Type", mimeType)
			req.Header.Set("Accept", "application/json")
			req.Header.Set("User-Agent", defaultUserAgent)
			return t.client.Do(req)
		}},
	}

	var resp *http.Response
	for _, strat := range strategies {
		resp, err = strat.fn()
		if err != nil {
			lastErr = fmt.Errorf("%s: send request: %w", strat.name, err)
			continue
		}
		if resp.StatusCode == 414 {
			bodyDump, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("%s: Pixhost returned status 414: %s", strat.name, strings.TrimSpace(string(bodyDump)))
			continue
		}
		break
	}

	if resp == nil {
		return "", fmt.Errorf("all upload strategies failed, last: %w", lastErr)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Pixhost returned status %d: %s", resp.StatusCode, string(body))
	}

	var result pixhostResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}

	imageURL := strings.TrimSpace(result.ImgURL)
	if imageURL == "" {
		if th := strings.TrimSpace(result.ThURL); th != "" {
			imageURL = pixhostThumbToFull(th)
		}
	}
	if imageURL == "" && strings.Contains(result.ShowURL, "/show/") {
		imageURL = strings.Replace(result.ShowURL, "/show/", "/images/", 1)
	}
	if imageURL == "" {
		return "", fmt.Errorf("Pixhost returned no image URL")
	}

	return imageURL, nil
}

var pixhostThRe = regexp.MustCompile(`^https?://t(\d+)\.pixhost\.to/thumbs/`)

func pixhostThumbToFull(thURL string) string {
	loc := pixhostThRe.FindStringIndex(thURL)
	if loc == nil {
		return thURL
	}
	sub := pixhostThRe.FindStringSubmatch(thURL)
	if len(sub) < 2 {
		return thURL
	}
	n := sub[1]
	return "https://img" + n + ".pixhost.to/images/" + thURL[loc[1]:]
}

// ─── ImgBB ────────────────────────────────────────────────────────────────────

const imgbbAPIURL = "https://api.imgbb.com/1/upload"

type imgbbResponse struct {
	Data struct {
		URL string `json:"url"`
	} `json:"data"`
	Status int             `json:"status"`
	Error  json.RawMessage `json:"error,omitempty"`
}

type imgbbKeyRing struct {
	mu    sync.Mutex
	keys  []string
	index int
}

func newImgbbKeyRing() *imgbbKeyRing {
	raw := os.Getenv("IMGBB_API_KEY")
	var keys []string
	for _, k := range strings.Split(raw, ",") {
		k = strings.TrimSpace(k)
		if k != "" {
			keys = append(keys, k)
		}
	}
	return &imgbbKeyRing{keys: keys}
}

func (kr *imgbbKeyRing) current() string {
	kr.mu.Lock()
	defer kr.mu.Unlock()
	if len(kr.keys) == 0 {
		return ""
	}
	return kr.keys[kr.index]
}

func (kr *imgbbKeyRing) rotate() {
	kr.mu.Lock()
	defer kr.mu.Unlock()
	if len(kr.keys) > 1 {
		kr.index = (kr.index + 1) % len(kr.keys)
	}
}

func (kr *imgbbKeyRing) count() int {
	kr.mu.Lock()
	defer kr.mu.Unlock()
	return len(kr.keys)
}

type ImgBBUploader struct {
	keys   *imgbbKeyRing
	client *http.Client
}

func NewImgBBUploader() *ImgBBUploader {
	return &ImgBBUploader{
		keys:   newImgbbKeyRing(),
		client: newNoProxyClient(60 * time.Second),
	}
}

func isRateLimitError(statusCode int, body []byte) bool {
	if statusCode == 429 {
		return true
	}
	return strings.Contains(strings.ToLower(string(body)), "rate limit")
}

func (u *ImgBBUploader) Upload(filePath string) (string, error) {
	if u.keys.count() == 0 {
		return "", fmt.Errorf("imgbb: IMGBB_API_KEY not set")
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("imgbb: read file: %w", err)
	}

	encoded := base64.StdEncoding.EncodeToString(data)

	attempts := u.keys.count()
	var lastErr error
	for i := 0; i < attempts; i++ {
		key := u.keys.current()
		form := url.Values{
			"key":   {key},
			"image": {encoded},
		}

		resp, err := u.client.PostForm(imgbbAPIURL, form)
		if err != nil {
			lastErr = fmt.Errorf("imgbb: post: %w", err)
			u.keys.rotate()
			continue
		}

		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
		resp.Body.Close()

		if readErr != nil {
			lastErr = fmt.Errorf("imgbb: read response: %w", readErr)
			u.keys.rotate()
			continue
		}

		if resp.StatusCode == 429 {
			lastErr = fmt.Errorf("imgbb: rate limited (HTTP 429)")
			u.keys.rotate()
			continue
		}

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("imgbb: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
			if isRateLimitError(resp.StatusCode, body) {
				u.keys.rotate()
				continue
			}
			return "", lastErr
		}

		var result imgbbResponse
		if err := json.Unmarshal(body, &result); err != nil {
			lastErr = fmt.Errorf("imgbb: parse response: %w", err)
			u.keys.rotate()
			continue
		}

		if result.Status != 200 {
			msg := string(result.Error)
			var errObj struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(result.Error, &errObj) == nil && errObj.Message != "" {
				msg = errObj.Message
			}
			if msg == "" || msg == "null" {
				msg = string(body)
			}
			err = fmt.Errorf("imgbb: error: %s", msg)
			lastErr = err
			if isRateLimitError(result.Status, []byte(msg)) {
				u.keys.rotate()
				continue
			}
			return "", err
		}

		if result.Data.URL == "" {
			return "", fmt.Errorf("imgbb: empty image URL in response")
		}

		return result.Data.URL, nil
	}

	return "", lastErr
}
