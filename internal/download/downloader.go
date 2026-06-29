package download

import (
	"fmt"
	"log"
	"os"
	"strings"
)

type Downloader interface {
	Download(url string, destPath string) error
}

func IdentifyHost(rawURL string) string {
	rawURL = strings.ToLower(rawURL)
	switch {
	case strings.Contains(rawURL, "streamtape.com"):
		return "Streamtape"
	case strings.Contains(rawURL, "gofile.io"):
		return "GoFile"
	case strings.Contains(rawURL, "voe.sx"):
		return "VOE.sx"
	case strings.Contains(rawURL, "mixdrop"):
		return "Mixdrop"
	case strings.Contains(rawURL, "seekstreaming"):
		return "SeekStreaming"
	default:
		return ""
	}
}

type Manager struct {
	streamtape *StreamtapeDownloader
	gofile     *GoFileDownloader
	voesx      *VoeSXDownloader
}

func NewManager(streamtapeLogin, streamtapeKey string) *Manager {
	return &Manager{
		streamtape: NewStreamtapeDownloader(streamtapeLogin, streamtapeKey),
		gofile:     NewGoFileDownloader(),
		voesx:      NewVoeSXDownloader(),
	}
}

func (m *Manager) DownloadFromAny(links []string, destPath string) error {
	priority := []string{"Streamtape", "GoFile", "VOE.sx"}
	hostMap := map[string]string{}
	for _, link := range links {
		host := IdentifyHost(link)
		if host != "" {
			hostMap[host] = link
		}
	}

	for _, host := range priority {
		link, ok := hostMap[host]
		if !ok {
			continue
		}

		log.Printf("Trying to download from %s: %s", host, truncateURL(link))

		var err error
		switch host {
		case "Streamtape":
			err = m.streamtape.Download(link, destPath)
		case "GoFile":
			err = m.gofile.Download(link, destPath)
		case "VOE.sx":
			err = m.voesx.Download(link, destPath)
		default:
			continue
		}

		if err == nil {
			if fi, statErr := os.Stat(destPath); statErr == nil && fi.Size() > 1024 {
				log.Printf("Downloaded from %s: %s (%d bytes)", host, destPath, fi.Size())
				return nil
			}
			if err == nil {
				err = fmt.Errorf("downloaded file is empty or too small")
			}
		}

		log.Printf("Download from %s failed: %v, trying next host", host, err)
	}

	return fmt.Errorf("all download hosts failed for %v", links)
}

func truncateURL(rawURL string) string {
	if len(rawURL) > 100 {
		return rawURL[:100] + "..."
	}
	return rawURL
}
