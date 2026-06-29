package main

import (
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
	deleteOrphans := flag.Bool("delete-orphans", true, "Delete recordings that have no upload_links")
	flag.Parse()

	supabaseURL := os.Getenv("SUPABASE_URL")
	supabaseKey := os.Getenv("SUPABASE_API_KEY")
	streamtapeLogin := os.Getenv("STREAMTAPE_LOGIN")
	streamtapeKey := os.Getenv("STREAMTAPE_KEY")

	if supabaseURL == "" || supabaseKey == "" {
		log.Fatal("SUPABASE_URL and SUPABASE_API_KEY environment variables required")
	}

	client := db.NewClient(supabaseURL, supabaseKey)
	dl := download.NewManager(streamtapeLogin, streamtapeKey)

	if *dryRun {
		log.Println("=== DRY RUN — no changes will be made ===")
	}

	log.Printf("Querying up to %d recordings without previews...", *batchSize)
	recordings, err := client.QueryRecordingsWithoutPreview(*batchSize)
	if err != nil {
		log.Fatalf("Failed to query recordings: %v", err)
	}

	log.Printf("Found %d recordings without previews", len(recordings))

	for _, rec := range recordings {
		log.Printf("--- Processing: %s (username: %s, timestamp: %s) ---",
			rec.Filename, rec.Username, rec.Timestamp)
		processRecording(client, dl, &rec, *dryRun, *deleteOrphans)
	}

	log.Println("Done.")
}

func processRecording(client *db.Client, dl *download.Manager, rec *db.Recording, dryRun, deleteOrphans bool) {
	links, err := client.GetUploadLinks(rec.ID)
	if err != nil {
		log.Printf("ERROR: failed to get upload links for %s: %v", rec.Filename, err)
		return
	}

	if len(links) == 0 {
		if deleteOrphans {
			log.Printf("No upload links for %s — deleting orphaned recording", rec.Filename)
			if !dryRun {
				_ = client.DeletePreviewImage(rec.Filename)
				if err := client.DeleteRecording(rec.Filename); err != nil {
					log.Printf("ERROR: failed to delete recording %s: %v", rec.Filename, err)
					return
				}
				log.Printf("Deleted recording: %s", rec.Filename)
			}
		} else {
			log.Printf("No upload links for %s — skipping (delete-orphans=false)", rec.Filename)
		}
		return
	}

	linkURLs := make([]string, len(links))
	for i, link := range links {
		linkURLs[i] = link.URL
	}
	log.Printf("Found %d upload link(s) for %s", len(links), rec.Filename)

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

	log.Printf("✓ Successfully processed %s", rec.Filename)
}

func emptyStr(s string) string {
	if s == "" {
		return "(empty)"
	}
	return s
}
