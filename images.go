package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// githubImageURL matches image URLs on the two hosts GitHub produces when an
// image is pasted or dragged into an issue: the current user-attachments assets
// endpoint (no file extension) and the legacy user-images host. It matches
// inside markdown `![](...)`, HTML `<img src="...">`, and bare URLs alike,
// because the surrounding syntax is not part of the capture. Trailing markdown
// or HTML delimiters are excluded from the URL by the character classes.
var githubImageURL = regexp.MustCompile(
	`https?://(?:github\.com/user-attachments/assets/[0-9a-fA-F-]+|user-images\.githubusercontent\.com/[^\s)"'<>]+)`)

// findGitHubImageURLs returns each distinct GitHub-hosted image URL in content,
// in first-appearance order.
func findGitHubImageURLs(content string) []string {
	matches := githubImageURL.FindAllString(content, -1)
	if matches == nil {
		return nil
	}
	seen := make(map[string]bool, len(matches))
	var urls []string
	for _, m := range matches {
		if !seen[m] {
			seen[m] = true
			urls = append(urls, m)
		}
	}
	return urls
}

// imageExt sniffs data with http.DetectContentType and returns the file
// extension for the supported image formats, or "" if data is not a supported
// image. The extension matters because the Read tool selects image rendering by
// file extension, and user-attachments URLs carry none.
func imageExt(data []byte) string {
	switch ct := http.DetectContentType(data); {
	case strings.HasPrefix(ct, "image/png"):
		return ".png"
	case strings.HasPrefix(ct, "image/jpeg"):
		return ".jpg"
	case strings.HasPrefix(ct, "image/gif"):
		return ".gif"
	case strings.HasPrefix(ct, "image/webp"):
		return ".webp"
	default:
		return ""
	}
}

// DownloadIssueImages downloads every GitHub-hosted image referenced in content
// to <destDir>/images/ and returns content with each such URL replaced by the
// absolute path of its saved file, so a Read-tool-enabled pipeline session can
// view the image. It is best-effort: any per-image failure is logged and
// skipped (its URL left in place), and it never returns an error, so a broken
// image never fails an issue. When content has no GitHub images it is returned
// unchanged without contacting gh.
func DownloadIssueImages(ctx context.Context, runner Runner, content, destDir string) string {
	urls := findGitHubImageURLs(content)
	if len(urls) == 0 {
		return content
	}
	token, _, err := runner.Run(ctx, "", nil, "", "gh", "auth", "token")
	if err != nil {
		log.Printf("images: gh auth token failed, skipping image download: %v", err)
		return content
	}
	token = strings.TrimSpace(token)

	imgDir := filepath.Join(destDir, "images")
	if err := os.MkdirAll(imgDir, 0o755); err != nil {
		log.Printf("images: cannot create %s, skipping image download: %v", imgDir, err)
		return content
	}

	downloaded := 0
	for i, url := range urls {
		data, _, err := runner.Run(ctx, "", nil, "", "curl",
			"-fsSL", "--max-time", "30",
			"-H", "Authorization: Bearer "+token, url)
		if err != nil {
			log.Printf("images: download failed for %s: %v", url, err)
			continue
		}
		ext := imageExt([]byte(data))
		if ext == "" {
			log.Printf("images: %s did not return a supported image, skipping", url)
			continue
		}
		path := filepath.Join(imgDir, "image-"+strconv.Itoa(i+1)+ext)
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			log.Printf("images: cannot write %s: %v", path, err)
			continue
		}
		content = strings.ReplaceAll(content, url, path)
		downloaded++
	}

	if downloaded == 0 {
		return content
	}
	note := fmt.Sprintf(
		"NOTE: this issue has %d attached image(s) saved locally; use the Read tool on the referenced paths to view them.\n\n",
		downloaded)
	return note + content
}
