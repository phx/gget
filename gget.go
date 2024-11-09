package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	CHUNK_SIZE      = 32 * 1024
	MAX_RETRY_COUNT = 3
)

type GGet struct {
	client       *http.Client
	headers      map[string]string
	cookies      []*http.Cookie
	skipSecurity bool
	quiet        bool
}

type DownloadConfig struct {
	URL          string
	Output       string
	Quiet        bool
	ID           string
	Fuzzy        bool
	Speed        string
	NoProgress   bool
	UseOriginal  bool
	SkipDownload bool
}

func NewGGet() *GGet {
	return &GGet{
		client: &http.Client{
			Timeout: 30 * time.Minute,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return nil
			},
		},
		headers: map[string]string{
			"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36",
		},
		skipSecurity: true,
		quiet:        false,
	}
}

// Improved URL parsing to handle more formats
func (g *GGet) extractFileID(urlStr string) string {
	// Handle direct ID input
	if !strings.Contains(urlStr, "/") && !strings.Contains(urlStr, "\\") {
		return urlStr
	}

	patterns := []string{
		`/file/d/([^/]+)`,
		`id=([^&]+)`,
		`/files/([^/]+)`,
		`/document/d/([^/]+)`,
		`/spreadsheets/d/([^/]+)`,
		`/presentation/d/([^/]+)`,
		`folders/([^/]+)`,
	}

	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		matches := re.FindStringSubmatch(urlStr)
		if len(matches) > 1 {
			return matches[1]
		}
	}

	// Try parsing as URL
	if parsedURL, err := url.Parse(urlStr); err == nil {
		queries := parsedURL.Query()
		if id := queries.Get("id"); id != "" {
			return id
		}
	}

	return ""
}

func (g *GGet) getConfirmToken(resp *http.Response) string {
	for _, cookie := range resp.Cookies() {
		if strings.HasPrefix(cookie.Name, "download_warning") {
			return cookie.Value
		}
	}
	return ""
}

func (g *GGet) getFileName(resp *http.Response, defaultName string) string {
	// Try Content-Disposition header
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if re := regexp.MustCompile(`filename\*?=(?:UTF-8'[^']*')?([^;]+)`); re.MatchString(cd) {
			matches := re.FindStringSubmatch(cd)
			if len(matches) > 1 {
				filename := strings.Trim(matches[1], `"'`)
				return filename
			}
		}
	}

	// Try URL path
	if resp.Request != nil && resp.Request.URL != nil {
		path := resp.Request.URL.Path
		if segments := strings.Split(path, "/"); len(segments) > 0 {
			lastSegment := segments[len(segments)-1]
			if lastSegment != "" {
				return lastSegment
			}
		}
	}

	return defaultName
}

func (g *GGet) downloadWithProgress(resp *http.Response, output string) error {
	out, err := os.Create(output + ".part") // Use .part extension while downloading
	if err != nil {
		return fmt.Errorf("failed to create output file: %v", err)
	}
	defer out.Close()

	fileSize := resp.ContentLength
	progress := 0
	lastProgressUpdate := time.Now()
	buffer := make([]byte, 32*1024) // 32KB buffer

	for {
		n, err := resp.Body.Read(buffer)
		if n > 0 {
			_, writeErr := out.Write(buffer[:n])
			if writeErr != nil {
				return fmt.Errorf("failed to write to file: %v", writeErr)
			}
			progress += n

			// Update progress every 100ms
			if !g.quiet && time.Since(lastProgressUpdate) > 100*time.Millisecond {
				if fileSize > 0 {
					percentage := float64(progress) / float64(fileSize) * 100
					fmt.Printf("\rDownloading... %.1f%% (%d/%d bytes)", percentage, progress, fileSize)
				} else {
					fmt.Printf("\rDownloading... %d bytes", progress)
				}
				lastProgressUpdate = time.Now()
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("download error: %v", err)
		}
	}

	if !g.quiet {
		fmt.Println() // New line after progress
	}

	// Rename .part file to final filename
	if err := os.Rename(output+".part", output); err != nil {
		return fmt.Errorf("failed to rename downloaded file: %v", err)
	}

	return nil
}

// Modify the getURLFromConfirmation function
func (g *GGet) getURLFromConfirmation(contents string) (string, error) {
	// Try finding the form first
	formRe := regexp.MustCompile(`<form.+?id="download-form".+?action="(.+?)"`)
	formMatches := formRe.FindStringSubmatch(contents)
	if len(formMatches) > 1 {
		formAction := formMatches[1]
		formAction = strings.Replace(formAction, "&amp;", "&", -1)

		// Extract hidden input values
		inputRe := regexp.MustCompile(`<input.+?name="([^"]+)".+?value="([^"]+)"`)
		inputs := inputRe.FindAllStringSubmatch(contents, -1)

		parsedURL, err := url.Parse(formAction)
		if err != nil {
			return "", err
		}

		query := parsedURL.Query()
		for _, input := range inputs {
			if len(input) == 3 && input[1] != "" {
				query.Set(input[1], input[2])
			}
		}

		parsedURL.RawQuery = query.Encode()
		return parsedURL.String(), nil
	}

	// Try the download link pattern
	re := regexp.MustCompile(`href="(\/uc\?export=download[^"]+)"`)
	matches := re.FindStringSubmatch(contents)
	if len(matches) > 1 {
		url := "https://docs.google.com" + matches[1]
		return strings.Replace(url, "&amp;", "&", -1), nil
	}

	// Try the JavaScript pattern
	re = regexp.MustCompile(`downloadUrl":"([^"]+)"`)
	matches = re.FindStringSubmatch(contents)
	if len(matches) > 1 {
		url := matches[1]
		url = strings.Replace(url, "\\u003d", "=", -1)
		url = strings.Replace(url, "\\u0026", "&", -1)
		return url, nil
	}

	// Check for error message
	re = regexp.MustCompile(`<p class="uc-error-subcaption">(.*?)</p>`)
	matches = re.FindStringSubmatch(contents)
	if len(matches) > 1 {
		return "", fmt.Errorf("drive error: %s", matches[1])
	}

	return "", fmt.Errorf("cannot retrieve the download link")
}

// Modify the downloadFile method
func (g *GGet) downloadFile(urlStr string, output string) error {
	fileID := g.extractFileID(urlStr)
	if fileID == "" {
		return fmt.Errorf("could not extract file ID from URL")
	}

	initialURL := fmt.Sprintf("https://drive.google.com/uc?id=%s&export=download", fileID)

	// First request to get the confirmation page
	req, err := http.NewRequest("GET", initialURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}

	for key, value := range g.headers {
		req.Header.Set(key, value)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %v", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %v", err)
	}
	bodyString := string(bodyBytes)

	var downloadURL string
	if strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		downloadURL, err = g.getURLFromConfirmation(bodyString)
		if err != nil {
			return fmt.Errorf("failed to get download URL: %v", err)
		}
	} else {
		downloadURL = initialURL
	}

	// Make the actual download request
	req, err = http.NewRequest("GET", downloadURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create download request: %v", err)
	}

	for key, value := range g.headers {
		req.Header.Set(key, value)
	}

	resp, err = g.client.Do(req)
	if err != nil {
		return fmt.Errorf("download request failed: %v", err)
	}
	defer resp.Body.Close()

	// Get or generate output filename
	if output == "" {
		output = g.getFileName(resp, fmt.Sprintf("gdrive_%s", fileID))
	}

	// Ensure the output directory exists
	if dir := filepath.Dir(output); dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create output directory: %v", err)
		}
	}

	return g.downloadWithProgress(resp, output)
}

func main() {
	var (
		outputFile = flag.String("o", "", "Output filename")
		quiet      = flag.Bool("q", false, "Quiet mode (no progress)")
		noCheck    = flag.Bool("no-check-certificate", false, "Skip certificate verification")
		version    = flag.Bool("V", false, "Show version")
		fileID     = flag.String("id", "", "Google Drive file ID")
	)

	flag.Parse()

	if *version {
		fmt.Println("gget version 1.0.0")
		return
	}

	downloader := NewGGet()
	downloader.quiet = *quiet

	// Handle certificate verification
	if *noCheck {
		downloader.client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}
	}

	var url string
	if *fileID != "" {
		url = *fileID
	} else if flag.NArg() > 0 {
		url = flag.Arg(0)
	} else {
		fmt.Println("Usage: gget [-o output_filename] [-q] [-id file_id] [-fuzzy] <google_drive_url>")
		os.Exit(1)
	}

	if err := downloader.downloadFile(url, *outputFile); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
