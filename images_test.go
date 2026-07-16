package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var errFake = errors.New("fake failure")

// pngBytes is a minimal byte slice that http.DetectContentType classifies as
// image/png (it only inspects the leading signature).
var pngBytes = []byte("\x89PNG\r\n\x1a\n0000000000000000")

func TestFindGitHubImageURLs(t *testing.T) {
	content := `# Bug

Markdown image: ![screenshot](https://github.com/user-attachments/assets/11111111-2222-3333-4444-555555555555)

HTML image: <img width="600" src="https://user-images.githubusercontent.com/1/abc.png" alt="x">

Bare url on its own line:
https://github.com/user-attachments/assets/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee

A non-GitHub image should be ignored: ![other](https://example.com/pic.png)

Duplicate of the first should not repeat: ![again](https://github.com/user-attachments/assets/11111111-2222-3333-4444-555555555555)
`
	got := findGitHubImageURLs(content)
	want := []string{
		"https://github.com/user-attachments/assets/11111111-2222-3333-4444-555555555555",
		"https://user-images.githubusercontent.com/1/abc.png",
		"https://github.com/user-attachments/assets/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d urls %v, want %d %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("url[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestFindGitHubImageURLsEmptyWhenNone(t *testing.T) {
	if got := findGitHubImageURLs("no images here, just https://example.com/x.png"); got != nil {
		t.Errorf("want nil, got %v", got)
	}
}

func TestImageExt(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"png", []byte("\x89PNG\r\n\x1a\n" + "rest of file"), ".png"},
		{"jpeg", []byte("\xff\xd8\xff\xe0\x00\x10JFIF" + "rest"), ".jpg"},
		{"gif", []byte("GIF89a" + "rest of file"), ".gif"},
		{"webp", []byte("RIFF\x00\x00\x00\x00WEBPVP8 "), ".webp"},
		{"text is not an image", []byte("<html>not an image</html>"), ""},
		{"empty", []byte(""), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := imageExt(c.data); got != c.want {
				t.Errorf("imageExt = %q, want %q", got, c.want)
			}
		})
	}
}

func TestDownloadIssueImagesRewritesAndSaves(t *testing.T) {
	url := "https://github.com/user-attachments/assets/11111111-2222-3333-4444-555555555555"
	content := "# Bug\n\n![shot](" + url + ")\n"
	dir := t.TempDir()

	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		if hasArg(c.args, "token") { // gh auth token
			return "gho_faketoken\n", "", nil
		}
		return string(pngBytes), "", nil // curl download
	}}

	got := DownloadIssueImages(context.Background(), f, content, dir)

	savedPath := filepath.Join(dir, "images", "image-1.png")
	if !strings.Contains(got, savedPath) {
		t.Errorf("content should reference saved path %q:\n%s", savedPath, got)
	}
	if strings.Contains(got, url) {
		t.Errorf("remote url should have been replaced:\n%s", got)
	}
	if !strings.Contains(got, "Read tool") {
		t.Errorf("expected a note pointing at the Read tool:\n%s", got)
	}
	data, err := os.ReadFile(savedPath)
	if err != nil {
		t.Fatalf("saved image not written: %v", err)
	}
	if string(data) != string(pngBytes) {
		t.Errorf("saved bytes = %q, want the downloaded png bytes", data)
	}
	// One gh auth token call + one curl call.
	if len(f.calls) != 2 {
		t.Fatalf("calls = %d, want 2 (gh auth token, curl)", len(f.calls))
	}
	curl := f.calls[1]
	if curl.name != "curl" {
		t.Errorf("second call = %q, want curl", curl.name)
	}
	if !hasArg(curl.args, url) {
		t.Errorf("curl args missing url: %v", curl.args)
	}
	if got := argAfter(curl.args, "-H"); !strings.Contains(got, "gho_faketoken") {
		t.Errorf("curl -H = %q, want it to carry the token", got)
	}
}

func TestDownloadIssueImagesNoImagesIsNoop(t *testing.T) {
	content := "# Bug\n\nNo images here.\n"
	f := &fakeRunner{}
	got := DownloadIssueImages(context.Background(), f, content, t.TempDir())
	if got != content {
		t.Errorf("content changed: %q", got)
	}
	if len(f.calls) != 0 {
		t.Fatalf("calls = %d, want 0 (no gh auth token when there are no images)", len(f.calls))
	}
}

func TestDownloadIssueImagesSkipsFailedDownload(t *testing.T) {
	good := "https://github.com/user-attachments/assets/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	bad := "https://user-images.githubusercontent.com/1/broken.png"
	content := "![a](" + good + ")\n![b](" + bad + ")\n"
	dir := t.TempDir()

	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		if hasArg(c.args, "token") {
			return "gho_faketoken\n", "", nil
		}
		if hasArg(c.args, bad) {
			return "", "curl: (22) 404", errFake
		}
		return string(pngBytes), "", nil
	}}

	got := DownloadIssueImages(context.Background(), f, content, dir)

	if !strings.Contains(got, filepath.Join(dir, "images", "image-1.png")) {
		t.Errorf("good image should be saved and referenced:\n%s", got)
	}
	if !strings.Contains(got, bad) {
		t.Errorf("failed image's url should be left untouched:\n%s", got)
	}
}

func TestDownloadIssueImagesSkipsNonImageBytes(t *testing.T) {
	url := "https://github.com/user-attachments/assets/11111111-2222-3333-4444-555555555555"
	content := "![x](" + url + ")\n"
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		if hasArg(c.args, "token") {
			return "gho_faketoken\n", "", nil
		}
		return "<html>login page, not an image</html>", "", nil
	}}
	got := DownloadIssueImages(context.Background(), f, content, t.TempDir())
	if !strings.Contains(got, url) {
		t.Errorf("non-image download should leave the url untouched:\n%s", got)
	}
}

func TestDownloadIssueImagesAuthFailureReturnsUnchanged(t *testing.T) {
	url := "https://github.com/user-attachments/assets/11111111-2222-3333-4444-555555555555"
	content := "![x](" + url + ")\n"
	f := &fakeRunner{handler: func(c rcall) (string, string, error) {
		if hasArg(c.args, "token") {
			return "", "not logged in", errFake
		}
		return string(pngBytes), "", nil
	}}
	got := DownloadIssueImages(context.Background(), f, content, t.TempDir())
	if got != content {
		t.Errorf("content should be unchanged when gh auth token fails:\n%s", got)
	}
	if len(f.calls) != 1 {
		t.Fatalf("calls = %d, want 1 (only the failed gh auth token)", len(f.calls))
	}
}
