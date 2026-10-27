package postproc

import (
	"archive/zip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// PrepareDownload snapshots the published editor package under the pipeline lock.
// Original uploads, database files and credentials are never included.
// Caller owns removal of the private temporary ZIP after streaming it.
func (p *Pipeline) PrepareDownload(takeID string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	dir, err := p.publishedDir(takeID)
	if err != nil {
		return "", err
	}
	var files []string
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symbolic links are not allowed in editor downloads")
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("non-regular package file")
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil || !filepath.IsLocal(rel) {
			return fmt.Errorf("invalid package path")
		}
		// Published outputs only; never include raw uploads or host configuration.
		if rel == "README.txt" || rel == "manifest.json" || filepath.Ext(rel) == ".fcpxml" || filepath.Ext(rel) == ".xml" || publishedFolder(rel) {
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		return "", fmt.Errorf("package manifest unavailable")
	}
	f, err := os.CreateTemp(p.Config.DataDir, ".editor-download-*.zip")
	if err != nil {
		return "", err
	}
	name := f.Name()
	success := false
	defer func() {
		f.Close()
		if !success {
			os.Remove(name)
		}
	}()
	archive := zip.NewWriter(f)
	for _, rel := range files {
		input, err := os.Open(filepath.Join(dir, rel))
		if err != nil {
			archive.Close()
			return "", err
		}
		info, err := input.Stat()
		if err != nil {
			input.Close()
			archive.Close()
			return "", err
		}
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			input.Close()
			archive.Close()
			return "", err
		}
		header.Name = filepath.ToSlash(rel)
		header.Method = zip.Store
		dest, err := archive.CreateHeader(header)
		if err == nil {
			_, err = io.Copy(dest, input)
		}
		input.Close()
		if err != nil {
			archive.Close()
			return "", err
		}
	}
	if err := archive.Close(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	success = true
	return name, nil
}

// publishedFolder reports whether rel sits in a deliverable folder: editor
// media, or the transcript and clip-editor handoff added after export
// (SL-P4-1). Hidden staging entries are never included.
func publishedFolder(rel string) bool {
	if strings.HasPrefix(filepath.Base(rel), ".") {
		return false
	}
	switch filepath.ToSlash(filepath.Dir(rel)) {
	case "video", "audio", "transcript", "handoff", "handoff/ave":
		return true
	}
	return false
}

// publishedDir resolves a complete take's published package directory.
// Callers hold p.mu.
func (p *Pipeline) publishedDir(takeID string) (string, error) {
	take, err := p.Store.GetTake(takeID)
	if err != nil || take.Status != "complete" {
		return "", fmt.Errorf("editor package is not complete")
	}
	session, err := p.Store.GetSession(take.SessionID)
	if err != nil {
		return "", fmt.Errorf("session unavailable: %w", err)
	}
	dir := p.exportDir(session, take)
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("published editor package unavailable")
	}
	return dir, nil
}

// PublishedPackageDir returns the published package of a complete take. A
// later export can replace the directory; use WithPublishedPackage to write.
func (p *Pipeline) PublishedPackageDir(takeID string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.publishedDir(takeID)
}

// WithPublishedPackage runs fn on the published package while holding the
// pipeline lock, so a concurrent export or download never sees a half-added
// transcript or handoff.
func (p *Pipeline) WithPublishedPackage(takeID string, fn func(dir string) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	dir, err := p.publishedDir(takeID)
	if err != nil {
		return err
	}
	return fn(dir)
}
