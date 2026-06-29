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
	minAge := flag.Duration("min-age", 10*time.Minute, "Skip recordings created within this duration (avoids race with active DVR pipeline)")
	nodeURL := flag.String("node-url", "", "DVR node web URL for downloading files (e.g. https://node.trycloudflare.com)")
	nodeUsername := flag.String("node-username", "", "Basic auth username for DVR node")
	nodePassword := flag.String("node-password", "", "Basic auth password for DVR node")
	flag.Parse()

	supabaseURL := os.Getenv("SUPABASE_URL")
	supabaseKey := os.Getenv("SUPABASE_API_KEY")
	streamtapeLogin := os.Getenv("STREAMTAPE_LOGIN")
	streamtapeKey := os.Getenv("STREAMTAPE_KEY")

	if supabaseURL == "" || supabaseKey == "" {
		log.Fatal("SUPABASE_URL and SUPABASE_API_KEY environment variables required")
	}

	if *nodeURL == "" {
		*nodeURL = os.Getenv("DVR_NODE_URL")
	}
	if *nodeUsername == "" {
		*nodeUsername = os.Getenv("DVR_NODE_USERNAME")
	}
	if *nodePassword == "" {
		*nodePassword = os.Getenv("DVR_NODE_PASSWORD")
	}

	client := db.NewClient(supabaseURL, supabaseKey)
	dl := download.NewManager(streamtapeLogin, streamtapeKey)

	var nodeDL *download.NodeDownloader
	if *nodeURL != "" {
		nodeDL = download.NewNodeDownloader(*nodeURL, *nodeUsername, *nodePassword)
	}

	if *dryRun {
		log.Println("=== DRY RUN — no changes will be made ===")
	}

	createdBefore := time.Now().UTC().Add(-*minAge).Format("2006-01-02T15:04:05Z")

	// --- Phase 1: recordings WITH upload links (existing flow) ---
	log.Printf("Phase 1: Querying up to %d recordings with upload links (created before %s)...", *batchSize, createdBefore)
	recordings, err := client.QueryRecordingsWithoutPreview(*batchSize, createdBefore)
	if err != nil {
		log.Printf("Phase 1 query failed: %v", err)
	} else {
		log.Printf("Phase 1: Found %d recordings with upload links", len(recordings))
		for _, rec := range recordings {
			log.Printf("--- Processing (phase 1): %s (username: %s) ---", rec.Filename, rec.Username)
			processRecording(client, dl, &rec, *dryRun)
		}
	}

	// --- Phase 2: recordings WITHOUT upload links (download from DVR node) ---
	if nodeDL == nil {
		log.Println("Phase 2: Skipped — no DVR node URL configured (set --node-url or DVR_NODE_URL)")
	} else {
		log.Printf("Phase 2: Querying up to %d recordings without upload links (created before %s)...", *batchSize, createdBefore)

		// Build file_path map from pipeline_states
		pipelineStates, err := client.QueryPipelineStates()
		if err != nil {
			log.Printf("Phase 2: failed to query pipeline_states: %v", err)
		} else {
			pathByFilename := map[string]string{}
			for _, ps := range pipelineStates {
				if ps.FilePath != "" && ps.FileSize > 0 {
					pathByFilename[ps.Filename] = ps.FilePath
				}
			}
			log.Printf("Phase 2: Found %d pipeline_states entries with valid file paths", len(pathByFilename))

			noLinkRecs, err := client.QueryRecordingsWithoutPreviewAny(*batchSize, createdBefore)
			if err != nil {
				log.Printf("Phase 2 query failed: %v", err)
			} else {
				log.Printf("Phase 2: Found %d recordings without preview (non-zero filesize)", len(noLinkRecs))
				for _, rec := range noLinkRecs {
					log.Printf("--- Processing (phase 2): %s (username: %s) ---", rec.Filename, rec.Username)
					processRecordingFromNode(client, nodeDL, &rec, pathByFilename, *dryRun)
				}
			}
		}
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

	generateUploadAndUpdate(client, rec.Filename, videoPath, dryRun)
}

func processRecordingFromNode(client *db.Client, nodeDL *download.NodeDownloader, rec *db.Recording, pathByFilename map[string]string, dryRun bool) {
	// Determine file path on the DVR node
	filePath, ok := pathByFilename[rec.Filename]
	if !ok {
		filePath = "D:\\videos\\" + rec.Filename
	}

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
		log.Printf("[DRY RUN] Would download from node %s to %s", filePath, videoPath)
		log.Printf("[DRY RUN] Would generate previews and update DB for %s", rec.Filename)
		return
	}

	log.Printf("Downloading from node: %s", filePath)
	if err := nodeDL.Download(filePath, videoPath); err != nil {
		log.Printf("ERROR: failed to download %s from node: %v", rec.Filename, err)
		return
	}

	generateUploadAndUpdate(client, rec.Filename, videoPath, dryRun)
}

func generateUploadAndUpdate(client *db.Client, filename, videoPath string, dryRun bool) {
	log.Printf("Generating previews...")
	thumbURL, spriteURL, previewURL := preview.GeneratePreviews(videoPath)

	if thumbURL == "" && spriteURL == "" && previewURL == "" {
		log.Printf("ERROR: all preview generation failed for %s", filename)
		return
	}

	log.Printf("Thumbnail: %s", emptyStr(thumbURL))
	log.Printf("Sprite: %s", emptyStr(spriteURL))
	log.Printf("Preview: %s", emptyStr(previewURL))

	if err := client.UpdateRecordingPreviewURLs(filename, thumbURL, spriteURL, previewURL); err != nil {
		log.Printf("ERROR: failed to update recording URLs: %v", err)
	}

	previewImg := &db.PreviewImage{
		Filename:     filename,
		ThumbnailURL: thumbURL,
		SpriteURL:    spriteURL,
		PreviewURL:   previewURL,
		UploadedAt:   time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	if err := client.SavePreviewImage(previewImg); err != nil {
		log.Printf("ERROR: failed to save preview image record: %v", err)
	}

	log.Printf("Successfully processed %s", filename)
}

func emptyStr(s string) string {
	if s == "" {
		return "(empty)"
	}
	return s
}
