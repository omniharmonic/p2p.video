package webassets

import (
	"embed"
	"io/fs"
)

// Embedded static web assets.
//
//go:embed dist/*
var embeddedFiles embed.FS

// FS returns an fs.FS sub-rooted at the dist folder.
func FS() fs.FS {
	sub, err := fs.Sub(embeddedFiles, "dist")
	if err != nil {
		return embeddedFiles
	}
	return sub
}
