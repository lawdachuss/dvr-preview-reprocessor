package preview

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/teacat/dvr-preview-reprocessor/internal/upload"
)

const (
	thumbWidth      = 1280
	thumbHeight     = 720
	spriteFrames    = 16
	spriteCols      = 4
	spriteRows      = 4
	spriteFrameW    = 640
	spriteFrameH    = 360
	previewWidth    = 320
	previewDuration = 6.0
	previewSegments = 12
)

var ffmpegSem = make(chan struct{}, 2)

func acquireFFmpeg() {
	ffmpegSem <- struct{}{}
}

func releaseFFmpeg() {
	<-ffmpegSem
}

func ffmpegCommandContext(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stderr = os.Stderr
	return cmd
}

func ffprobeCommandContext(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "ffprobe", args...)
	return cmd
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

func GeneratePreviews(videoPath string) (thumbURL, spriteURL, previewURL string) {
	ext := strings.ToLower(filepath.Ext(videoPath))
	if ext != ".mp4" && ext != ".mkv" && ext != ".ts" {
		return "", "", ""
	}

	st, err := os.Stat(videoPath)
	if err != nil {
		log.Printf("file not found %s: %v", filepath.Base(videoPath), err)
		return "", "", ""
	}
	if st.Size() < 100*1024 {
		log.Printf("skipping %s: too small (%d bytes)", filepath.Base(videoPath), st.Size())
		return "", "", ""
	}

	baseName := filepath.Base(videoPath)

	probeCtx, probeCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer probeCancel()

	var dur float64
	acquireFFmpeg()
	probeOut, probeErr := ffprobeCommandContext(probeCtx,
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		videoPath,
	).Output()
	releaseFFmpeg()
	if probeErr == nil {
		dur, _ = strconv.ParseFloat(strings.TrimSpace(string(probeOut)), 64)
	}

	interval := 10.0
	if dur > 0 {
		interval = dur / float64(spriteFrames)
		if interval < 0.1 {
			interval = 0.1
		}
	}

	thumbDone := make(chan string, 1)
	spriteDone := make(chan string, 1)
	previewDone := make(chan string, 1)

	// ── Thumbnail ──
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("PANIC [thumb] %s: %v", baseName, r)
				select {
				case thumbDone <- "":
				default:
				}
			}
		}()

		thumbCtx, thumbCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer thumbCancel()

		thumbJPG := videoPath + ".thumb.jpg"
		defer os.Remove(thumbJPG)

		seekPos := "00:00:03"
		if dur > 0 && dur < 3 {
			seekPos = fmt.Sprintf("%.2f", dur*0.5)
		} else if dur > 0 {
			seekPos = fmt.Sprintf("%.2f", dur*0.1)
		}

		acquireFFmpeg()
		err := ffmpegCommandContext(thumbCtx,
			"-y",
			"-ss", seekPos,
			"-i", videoPath,
			"-vframes", "1",
			"-vf", fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2",
				thumbWidth, thumbHeight, thumbWidth, thumbHeight),
			"-c:v", "mjpeg",
			"-q:v", "5",
			thumbJPG,
		).Run()
		releaseFFmpeg()

		if err != nil {
			log.Printf("thumb: failed for %s: %v", baseName, err)
			thumbDone <- ""
			return
		}

		imgUploader := upload.NewMultiImageUploader()
		if remoteURL, _, uploadErr := imgUploader.Upload(thumbJPG); uploadErr == nil {
			log.Printf("thumb: ✓ %s", baseName)
			thumbDone <- remoteURL
		} else {
			log.Printf("thumb: upload failed for %s: %v", baseName, uploadErr)
			thumbDone <- ""
		}
	}()

	// ── Sprite ──
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("PANIC [sprite] %s: %v", baseName, r)
				select {
				case spriteDone <- "":
				default:
				}
			}
		}()

		spriteCtx, spriteCancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer spriteCancel()

		spriteJPG := videoPath + ".sprite.jpg"
		defer os.Remove(spriteJPG)

		vf := fmt.Sprintf(
			"fps=1/%.4f,scale=%d:%d:force_original_aspect_ratio=decrease:flags=lanczos,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,tile=%dx%d",
			interval,
			spriteFrameW, spriteFrameH,
			spriteFrameW, spriteFrameH,
			spriteCols, spriteRows,
		)

		acquireFFmpeg()
		err := ffmpegCommandContext(spriteCtx,
			"-y",
			"-i", videoPath,
			"-vf", vf,
			"-frames:v", "1",
			"-c:v", "mjpeg",
			"-q:v", "5",
			spriteJPG,
		).Run()
		releaseFFmpeg()

		if err != nil {
			log.Printf("sprite: failed for %s: %v", baseName, err)
			spriteDone <- ""
			return
		}

		imgUploader := upload.NewMultiImageUploader()
		if remoteURL, _, uploadErr := imgUploader.Upload(spriteJPG); uploadErr == nil {
			log.Printf("sprite: ✓ %s", baseName)
			spriteDone <- remoteURL
		} else {
			log.Printf("sprite: upload failed for %s: %v", baseName, uploadErr)
			spriteDone <- ""
		}
	}()

	// ── Preview MP4 ──
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("PANIC [preview] %s: %v", baseName, r)
				select {
				case previewDone <- "":
				default:
				}
			}
		}()

		previewCtx, previewCancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer previewCancel()

		previewMP4 := videoPath + ".preview.mp4"
		var previewGenerated bool
		defer func() {
			if previewGenerated {
				os.Remove(previewMP4)
			}
		}()

		waitForPreviewFile := func() bool {
			for delay := 0; delay < 8; delay++ {
				if fileExists(previewMP4) {
					return true
				}
				time.Sleep(time.Duration(50*(1<<delay)) * time.Millisecond)
			}
			return false
		}

		generatePreview := func(ctx context.Context) bool {
			var err error
			if dur <= previewDuration || dur <= 0 {
				acquireFFmpeg()
				err = ffmpegCommandContext(ctx,
					"-y",
					"-i", videoPath,
					"-vf", fmt.Sprintf("scale=%d:-2:flags=lanczos", previewWidth),
					"-c:v", "libx264",
					"-preset", "fast",
					"-crf", "23",
					"-movflags", "+faststart",
					"-an",
					previewMP4,
				).Run()
				releaseFFmpeg()
			} else {
				segDuration := previewDuration / float64(previewSegments)
				step := dur / float64(previewSegments)

				var filterParts []string
				var concatInputs []string

				for i := 0; i < previewSegments; i++ {
					midpoint := step * (float64(i) + 0.5)
					start := midpoint - segDuration/2

					if start+segDuration > dur {
						start = dur - segDuration
					}
					if start < 0 {
						start = 0
					}

					label := fmt.Sprintf("v%d", i)
					filterParts = append(filterParts, fmt.Sprintf(
						"[0:v]trim=start=%.3f:duration=%.3f,setpts=PTS-STARTPTS,scale=%d:-2:flags=lanczos[%s]",
						start, segDuration, previewWidth, label,
					))
					concatInputs = append(concatInputs, fmt.Sprintf("[%s]", label))
				}

				filterComplex := strings.Join(filterParts, ";") + ";" +
					strings.Join(concatInputs, "") +
					fmt.Sprintf("concat=n=%d:v=1:a=0[out]", previewSegments)

				acquireFFmpeg()
				err = ffmpegCommandContext(ctx,
					"-y",
					"-i", videoPath,
					"-filter_complex", filterComplex,
					"-map", "[out]",
					"-c:v", "libx264",
					"-preset", "fast",
					"-crf", "23",
					"-movflags", "+faststart",
					"-an",
					previewMP4,
				).Run()
				releaseFFmpeg()

				if err != nil || !fileExists(previewMP4) {
					if err != nil {
						log.Printf("preview: complex filter failed for %s: %v, trying simple fallback", baseName, err)
					} else {
						log.Printf("preview: complex filter produced no output for %s, trying simple fallback", baseName)
					}
					fallbackCtx, fallbackCancel := context.WithTimeout(context.Background(), 5*time.Minute)
					defer fallbackCancel()
					acquireFFmpeg()
					err = ffmpegCommandContext(fallbackCtx,
						"-y",
						"-ss", fmt.Sprintf("%.2f", dur*0.3),
						"-i", videoPath,
						"-t", fmt.Sprintf("%.2f", previewDuration),
						"-vf", fmt.Sprintf("scale=%d:-2:flags=lanczos", previewWidth),
						"-c:v", "libx264",
						"-preset", "fast",
						"-crf", "23",
						"-movflags", "+faststart",
						"-an",
						previewMP4,
					).Run()
					releaseFFmpeg()
				}
			}

			if err != nil {
				log.Printf("preview: ffmpeg failed for %s: %v", baseName, err)
				return false
			}

			if !waitForPreviewFile() {
				log.Printf("preview: ffmpeg exited successfully but produced no output file for %s", baseName)
				return false
			}

			return true
		}

		acquireFFmpeg()
		previewOK := generatePreview(previewCtx)
		releaseFFmpeg()
		if !previewOK {
			previewDone <- ""
			return
		}
		previewGenerated = true

		catboxUploader := upload.NewCatboxUploader()
		pixeldrainUploader := upload.NewPixelDrainUploader(os.Getenv("PIXELDRAIN_API_KEY"))
		lobfileUploader := upload.NewLobFileUploader(os.Getenv("LOBFILE_API_KEY"))
		keepshUploader := upload.NewKeepShUploader()

		var remoteURL string
		var uploadErr error

		maxPreviewAttempts := 2
		for attempt := 0; attempt < maxPreviewAttempts; attempt++ {
			if attempt > 0 {
				log.Printf("preview: regenerating %s (attempt %d/%d)", baseName, attempt+1, maxPreviewAttempts)
				regenCtx, regenCancel := context.WithTimeout(context.Background(), 5*time.Minute)
				acquireFFmpeg()
				ok := generatePreview(regenCtx)
				releaseFFmpeg()
				regenCancel()
				if !ok {
					uploadErr = fmt.Errorf("preview regeneration failed")
					break
				}
			}

			remoteURL, uploadErr = catboxUploader.Upload(previewMP4)
			if uploadErr == nil {
				break
			}
			log.Printf("preview: catbox failed for %s: %v, trying PixelDrain", baseName, uploadErr)

			remoteURL, uploadErr = pixeldrainUploader.Upload(previewMP4)
			if uploadErr == nil {
				break
			}
			log.Printf("preview: PixelDrain failed for %s: %v, trying LobFile", baseName, uploadErr)

			remoteURL, uploadErr = lobfileUploader.Upload(previewMP4)
			if uploadErr == nil {
				break
			}
			log.Printf("preview: LobFile failed for %s: %v", baseName, uploadErr)

			remoteURL, uploadErr = keepshUploader.Upload(previewMP4)
			if uploadErr == nil {
				break
			}
			log.Printf("preview: Keep.sh failed for %s: %v", baseName, uploadErr)

			errStr := uploadErr.Error()
			if strings.Contains(errStr, "no such file") ||
				strings.Contains(errStr, "cannot find") ||
				strings.Contains(errStr, "stat file") ||
				strings.Contains(errStr, "open file") {
				continue
			}
			break
		}

		if uploadErr == nil {
			log.Printf("preview: ✓ %s", baseName)
			previewDone <- remoteURL
		} else {
			log.Printf("preview: all hosts failed for %s: %v", baseName, uploadErr)
			previewDone <- ""
		}
	}()

	thumbURL = <-thumbDone
	spriteURL = <-spriteDone
	previewURL = <-previewDone

	return thumbURL, spriteURL, previewURL
}


