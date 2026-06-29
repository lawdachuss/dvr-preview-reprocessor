package main

import (
	"encoding/json"
	"flag"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/teacat/dvr-preview-reprocessor/internal/db"
	"github.com/teacat/dvr-preview-reprocessor/internal/download"
	"github.com/teacat/dvr-preview-reprocessor/internal/preview"
)

func main() {
	batchSize := flag.Int("batch-size", 5, "Number of recordings to process per run")
	dryRun := flag.Bool("dry-run", false, "Scan and log only, no mutations")
	minAge := flag.Duration("min-age", 10*time.Minute, "Skip recordings created within this duration (avoids race with active DVR pipeline)")
	flag.Parse()

	supabaseURL := os.Getenv("SUPABASE_URL")
	supabaseKey := os.Getenv("SUPABASE_API_KEY")
	streamtapeLogin := os.Getenv("STREAMTAPE_LOGIN")
	streamtapeKey := os.Getenv("STREAMTAPE_KEY")

	if supabaseURL == "" || supabaseKey == "" {
		log.Fatal("SUPABASE_URL and SUPABASE_API_KEY environment variables required")
	}

	client := db.NewClient(supabaseURL, supabaseKey)

	// Debug: check what recordings_with_links actually contains
	type RawRec struct {
		ID         string `json:"id"`
		Filename   string `json:"filename"`
		PreviewURL string `json:"preview_url"`
		Links      json.RawMessage `json:"links"`
	}
	var rawRecs []RawRec
	err := client.GetRaw("/recordings_with_links?order=timestamp.desc&limit=5", &rawRecs)
	if err != nil {
		log.Printf("DEBUG: failed raw query: %v", err)
	} else {
		for _, r := range rawRecs {
			log.Printf("DEBUG: id=%s filename=%s preview_url=%q links=%s", r.ID, r.Filename, r.PreviewURL, string(r.Links))
		}
	}

	dl := download.NewManager(streamtapeLogin, streamtapeKey)

	if *dryRun {
		log.Println("=== DRY RUN — no changes will be made ===")
	}

	createdBefore := time.Now().UTC().Add(-*minAge).Format("2006-01-02T15:04:05Z")
	log.Printf("Querying up to %d recordings without previews (created before %s)...", *batchSize, createdBefore)
	recordings, err := client.QueryRecordingsWithoutPreview(*batchSize, createdBefore)
	if err != nil {
		log.Fatalf("Failed to query recordings: %v", err)
	}

	log.Printf("Found %d recordings without previews (with upload links)", len(recordings))

	for _, rec := range recordings {
		log.Printf("--- Processing: %s (username: %s, timestamp: %s) ---",
			rec.Filename, rec.Username, rec.Timestamp)
		processRecording(client, dl, &rec, *dryRun)
	}

	log.Println("Done.")
}

func processRecording(client *db.Client, dl *download.Manager, rec *db.Recording, dryRun bool) {
	if len(rec.Links) == 0 {
		log.Printf("No upload links for %s — skipping", rec.Filename)
		return
	}

	linkURLs := make([]string, 0, len(rec.Links))
	for _, url := range rec.Links {
		linkURLs = append(linkURLs, url)
	}
	log.Printf("Found %d upload link(s) for %s", len(linkURLs), rec.Filename)

	existing, err := client.GetPreviewImage(rec.Filename)
	if err != nil {
		log.Printf("WARNING: failed to check preview_images for %s: %v", rec.Filename, err)
	}
	if existing != nil && existing.PreviewURL != "" {
		log.Printf("preview_images already has preview_url for %s — copying to recordings table", rec.Filename)
		if !dryRun {
			if err := client.UpdateRecordingPreviewURLs(rec.Filename, existing.ThumbnailURL, existing.SpriteURL, existing.PreviewURL); err != nil {
				log.Printf("ERROR: failed to update recording URLs from preview_images: %v", err)
			}
		}
		log.Printf("Copied existing preview URLs for %s", rec.Filename)
		return
	}

	workDir, err := os.MkdirTemp("", "dvr-reprocess-*")
	if err != nil {
		log.Printf("ERROR: failed to create temp dir: %v", err)
		return
	}
	defer os.RemoveAll(workDir)

	videoPath := filepath.Join(workDir, rec.Filename)
	ext := strings.ToLower(filepath.Ext(videoPath))
	if ext != ".mp4" && ext != ".mkv" && ext != ".ts" {
		videoPath += ".mp4"
	}

	if dryRun {
		log.Printf("[DRY RUN] Would download from %v to %s", linkURLs, videoPath)
		log.Printf("[DRY RUN] Would generate previews and update DB for %s", rec.Filename)
		return
	}

	log.Printf("Downloading video...")
	if err := dl.DownloadFromAny(linkURLs, videoPath); err != nil {
		log.Printf("ERROR: failed to download %s: %v", rec.Filename, err)
		return
	}

	log.Printf("Generating previews...")
	thumbURL, spriteURL, previewURL := preview.GeneratePreviews(videoPath)

	if thumbURL == "" && spriteURL == "" && previewURL == "" {
		log.Printf("ERROR: all preview generation failed for %s", rec.Filename)
		return
	}

	log.Printf("Thumbnail: %s", emptyStr(thumbURL))
	log.Printf("Sprite: %s", emptyStr(spriteURL))
	log.Printf("Preview: %s", emptyStr(previewURL))

	if err := client.UpdateRecordingPreviewURLs(rec.Filename, thumbURL, spriteURL, previewURL); err != nil {
		log.Printf("ERROR: failed to update recording URLs: %v", err)
	}

	previewImg := &db.PreviewImage{
		Filename:     rec.Filename,
		ThumbnailURL: thumbURL,
		SpriteURL:    spriteURL,
		PreviewURL:   previewURL,
		UploadedAt:   time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	if err := client.SavePreviewImage(previewImg); err != nil {
		log.Printf("ERROR: failed to save preview image record: %v", err)
	}

	log.Printf("Successfully processed %s", rec.Filename)
}

func emptyStr(s string) string {
	if s == "" {
		return "(empty)"
	}
	return s
}
