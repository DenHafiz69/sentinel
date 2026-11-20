package main

// Deliverable extraction: mirrors Hermes gateway "deliverable mode".
// Scan the agent reply for absolute/home-relative file paths with supported
// extensions, skipping anything inside fenced or inline code, and turn the
// ones that actually exist on disk into chat attachments.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var catByExt = map[string]string{
	// images (svg is NOT inline-rendered; it downloads, to avoid same-origin script execution)
	".png": "image", ".jpg": "image", ".jpeg": "image", ".gif": "image",
	".webp": "image", ".bmp": "image", ".tiff": "image", ".tif": "image",
	// video
	".mp4": "video", ".mov": "video", ".avi": "video", ".mkv": "video",
	".webm": "video", ".3gp": "video",
	// audio
	".mp3": "audio", ".m2a": "audio", ".wav": "audio", ".ogg": "audio",
	".opus": "audio", ".m4a": "audio", ".flac": "audio",
	// documents / data / archives / web / source
	".pdf": "file", ".docx": "file", ".doc": "file", ".odt": "file", ".rtf": "file",
	".txt": "file", ".md": "file", ".epub": "file",
	".xlsx": "file", ".xls": "file", ".ods": "file", ".csv": "file", ".tsv": "file",
	".json": "file", ".xml": "file", ".yaml": "file", ".yml": "file", ".ipynb": "file",
	".kmz": "file", ".kml": "file", ".geojson": "file", ".gpx": "file",
	".pptx": "file", ".ppt": "file", ".odp": "file",
	".zip": "file", ".tar": "file", ".gz": "file", ".tgz": "file", ".bz2": "file",
	".xz": "file", ".7z": "file", ".rar": "file",
	".html": "file", ".htm": "file", ".svg": "file",
	".go": "file", ".py": "file", ".sh": "file", ".sql": "file",
	".service": "file", ".conf": "file", ".log": "file",
	// note: .env and other unknown extensions are deliberately NOT deliverable
}

var pathRe = regexp.MustCompile(
	`(?:~/(?:[A-Za-z0-9._@+-]+/)*[A-Za-z0-9._@+-]+|/(?:home|mnt|tmp|var|opt|usr)/(?:[A-Za-z0-9._@+-]+/)*[A-Za-z0-9._@+-]+)`)

var inlineCodeRe = regexp.MustCompile("`[^`\n]*`")

// categoryFor reports the attachment category for a path, and whether its
// extension is on the deliverable allowlist at all.
func categoryFor(path string) (string, bool) {
	ext := strings.ToLower(filepath.Ext(path))
	k, ok := catByExt[ext]
	return k, ok
}

// kindForFile is the lenient version used for user uploads: known media
// categories keep their kind, everything else is a plain file.
func kindForFile(path string) string {
	if k, ok := categoryFor(path); ok && k != "" {
		return k
	}
	return "file"
}

func stripCodeBlocks(text string) string {
	var b strings.Builder
	inFence := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if !inFence {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return inlineCodeRe.ReplaceAllString(b.String(), " ")
}

func extractAttachments(text string) []Attachment {
	clean := stripCodeBlocks(text)
	seen := map[string]bool{}
	var out []Attachment
	for _, m := range pathRe.FindAllString(clean, -1) {
		p := strings.TrimRight(m, ".,;:!?)]}'\"")
		if strings.HasPrefix(p, "~/") {
			p = filepath.Join(homeDir, p[2:])
		}
		p = filepath.Clean(p)
		kind, ok := categoryFor(p)
		if !ok || seen[p] {
			continue
		}
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
			continue
		}
		seen[p] = true
		out = append(out, Attachment{
			Path: p,
			Name: filepath.Base(p),
			Size: fi.Size(),
			Kind: kind,
		})
		if len(out) >= 8 {
			break
		}
	}
	return out
}
