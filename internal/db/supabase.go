package db

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	BaseURL string
	APIKey  string
	http    *http.Client
}

func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		http:    &http.Client{Timeout: 60 * time.Second},
	}
}

type Recording struct {
	ID           string          `json:"id,omitempty"`
	ChannelID    string          `json:"channel_id,omitempty"`
	Username     string          `json:"username"`
	Filename     string          `json:"filename"`
	Timestamp    string          `json:"timestamp"`
	RoomTitle    string          `json:"room_title,omitempty"`
	Tags         []string        `json:"tags,omitempty"`
	Viewers      int             `json:"viewers"`
	Resolution   string          `json:"resolution,omitempty"`
	Framerate    int             `json:"framerate"`
	Filesize     int64           `json:"filesize"`
	Duration     float64         `json:"duration,omitempty"`
	Gender       string          `json:"gender,omitempty"`
	ThumbnailURL string          `json:"thumbnail_url,omitempty"`
	SpriteURL    string          `json:"sprite_url,omitempty"`
	PreviewURL   string          `json:"preview_url,omitempty"`
	EmbedURL     string          `json:"embed_url,omitempty"`
	InstanceID   string          `json:"instance_id,omitempty"`
	CreatedAt    string          `json:"created_at,omitempty"`
	UpdatedAt    string          `json:"updated_at,omitempty"`
	Links        json.RawMessage `json:"links,omitempty"`
}

type UploadLink struct {
	ID          string `json:"id,omitempty"`
	RecordingID string `json:"recording_id"`
	Host        string `json:"host"`
	URL         string `json:"url"`
	UploadedAt  string `json:"uploaded_at,omitempty"`
}

type PreviewImage struct {
	ID           string `json:"id,omitempty"`
	RecordingID  string `json:"recording_id,omitempty"`
	Filename     string `json:"filename"`
	ThumbnailURL string `json:"thumbnail_url,omitempty"`
	SpriteURL    string `json:"sprite_url,omitempty"`
	PreviewURL   string `json:"preview_url,omitempty"`
	InstanceID   string `json:"instance_id,omitempty"`
	UploadedAt   string `json:"uploaded_at,omitempty"`
}

func (c *Client) request(method, path string, body interface{}) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		jsonData, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(jsonData)
	}

	req, err := http.NewRequest(method, c.BaseURL+"/rest/v1"+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("apikey", c.APIKey)
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Prefer", "resolution=merge-duplicates,return=representation")
	}

	return c.http.Do(req)
}

func (c *Client) get(path string, result interface{}) error {
	resp, err := c.request("GET", path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}

	return json.NewDecoder(resp.Body).Decode(result)
}

func (c *Client) patch(path string, body interface{}) error {
	resp, err := c.request("PATCH", path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

func (c *Client) delete(path string) error {
	resp, err := c.request("DELETE", path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

func (c *Client) QueryRecordingsWithoutPreview(limit int, createdBefore string) ([]Recording, error) {
	filter := "or=(preview_url.is.null,preview_url.eq.)"
	if createdBefore != "" {
		filter = fmt.Sprintf("or=(preview_url.is.null,preview_url.eq.)&created_at=lt.%s", url.QueryEscape(createdBefore))
	}
	path := fmt.Sprintf("/recordings?%s&order=timestamp.desc&limit=%d", filter, limit)
	var recordings []Recording
	err := c.get(path, &recordings)
	return recordings, err
}

func (c *Client) GetRecording(filename string) (*Recording, error) {
	var recordings []Recording
	err := c.get(fmt.Sprintf("/recordings?filename=eq.%s&limit=1", url.QueryEscape(filename)), &recordings)
	if err != nil {
		return nil, err
	}
	if len(recordings) == 0 {
		return nil, fmt.Errorf("recording not found")
	}
	return &recordings[0], nil
}

func (c *Client) GetUploadLinks(recordingID string) ([]UploadLink, error) {
	var links []UploadLink
	err := c.get(fmt.Sprintf("/upload_links?recording_id=eq.%s", url.QueryEscape(recordingID)), &links)
	return links, err
}

func (c *Client) GetPreviewImage(filename string) (*PreviewImage, error) {
	var images []PreviewImage
	err := c.get(fmt.Sprintf("/preview_images?filename=eq.%s&limit=1", url.QueryEscape(filename)), &images)
	if err != nil {
		return nil, err
	}
	if len(images) == 0 {
		return nil, nil
	}
	return &images[0], nil
}

func (c *Client) UpdateRecordingPreviewURLs(filename, thumbnailURL, spriteURL, previewURL string) error {
	body := map[string]string{
		"thumbnail_url": thumbnailURL,
		"sprite_url":    spriteURL,
		"preview_url":   previewURL,
	}
	return c.patch(fmt.Sprintf("/recordings?filename=eq.%s", url.QueryEscape(filename)), body)
}

func (c *Client) SavePreviewImage(img *PreviewImage) error {
	resp, err := c.request("POST", "/preview_images?on_conflict=filename", img)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

func (c *Client) DeleteRecording(filename string) error {
	return c.delete(fmt.Sprintf("/recordings?filename=eq.%s", url.QueryEscape(filename)))
}

func (c *Client) DeletePreviewImage(filename string) error {
	return c.delete(fmt.Sprintf("/preview_images?filename=eq.%s", url.QueryEscape(filename)))
}

func (c *Client) DeleteUploadLinksByRecordingID(recordingID string) error {
	return c.delete(fmt.Sprintf("/upload_links?recording_id=eq.%s", url.QueryEscape(recordingID)))
}

func (c *Client) DebugQueryLinks(limit int) ([]Recording, error) {
	path := fmt.Sprintf("/recordings_with_links?select=filename,links&order=created_at.desc&limit=%d", limit)
	var recordings []Recording
	err := c.get(path, &recordings)
	return recordings, err
}
